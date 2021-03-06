// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main // import "golang.org/x/build/cmd/coordinator/buildongce"

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	compute "google.golang.org/api/compute/v1"
	"google.golang.org/api/googleapi"
)

var (
	proj        = flag.String("project", "symbolic-datum-552", "name of Project")
	zone        = flag.String("zone", "us-central1-f", "GCE zone")
	mach        = flag.String("machinetype", "n1-standard-4", "Machine type")
	instName    = flag.String("instance_name", "farmer", "Name of VM instance.")
	sshPub      = flag.String("ssh_public_key", "", "ssh public key file to authorize. Can modify later in Google's web UI anyway.")
	staticIP    = flag.String("static_ip", "", "Static IP to use. If empty, automatic.")
	reuseDisk   = flag.Bool("reuse_disk", true, "Whether disk images should be reused between shutdowns/restarts.")
	ssd         = flag.Bool("ssd", true, "use a solid state disk (faster, more expensive)")
	coordinator = flag.String("coord", "https://storage.googleapis.com/go-builder-data/coordinator", "Coordinator binary URL")
	staging     = flag.Bool("staging", false, "change default -project and -coordinator flags to their default dev cluster values, as well as use 'staging-' prefixed OAuth token files.")
)

func stagingPrefix() string {
	if *staging {
		return "staging-"
	}
	return ""
}

func readFile(v string) string {
	slurp, err := ioutil.ReadFile(v)
	if err != nil {
		log.Fatalf("Error reading %s: %v", v, err)
	}
	return strings.TrimSpace(string(slurp))
}

const baseConfig = `#cloud-config
coreos:
  update:
    group: stable
    reboot-strategy: off
  units:
    - name: gobuild.service
      command: start
      content: |
        [Unit]
        Description=Go Builders
        After=docker.service
        Requires=docker.service
        
        [Service]
        ExecStartPre=/bin/bash -c 'mkdir -p /opt/bin && curl -s -o /opt/bin/coordinator.tmp $COORDINATOR && install -m 0755 /opt/bin/coordinator{.tmp,}'
        ExecStart=/opt/bin/coordinator
        RestartSec=10s
        Restart=always
        StartLimitInterval=0
        Type=simple
        
        [Install]
        WantedBy=multi-user.target
`

func main() {
	flag.Parse()

	if *staging {
		if *proj == "symbolic-datum-552" {
			*proj = "go-dashboard-dev"
		}
		if *coordinator == "https://storage.googleapis.com/go-builder-data/coordinator" {
			*coordinator = "https://storage.googleapis.com/dev-go-builder-data/coordinator"
		}
	}
	if *proj == "" {
		log.Fatalf("Missing --project flag")
	}
	if *staticIP == "" {
		// Hard-code this, since GCP doesn't let you rename an IP address, and so
		// this IP is still called "go-builder-1-ip" in our project, from our old
		// naming convention plan.
		switch *proj {
		case "symbolic-datum-552":
			*staticIP = "107.178.219.46"
		case "go-dashboard-dev":
			*staticIP = "104.154.113.235"
		}
	}
	prefix := "https://www.googleapis.com/compute/v1/projects/" + *proj
	machType := prefix + "/zones/" + *zone + "/machineTypes/" + *mach

	oauthClient := oauth2.NewClient(oauth2.NoContext, tokenSource())

	computeService, _ := compute.New(oauthClient)

	natIP := *staticIP
	if natIP == "" {
		// Try to find it by name.
		aggAddrList, err := computeService.Addresses.AggregatedList(*proj).Do()
		if err != nil {
			log.Fatal(err)
		}
		// https://godoc.org/google.golang.org/api/compute/v1#AddressAggregatedList
	IPLoop:
		for _, asl := range aggAddrList.Items {
			for _, addr := range asl.Addresses {
				if addr.Name == *instName+"-ip" && addr.Status == "RESERVED" {
					natIP = addr.Address
					break IPLoop
				}
			}
		}
	}

	cloudConfig := strings.Replace(baseConfig, "$COORDINATOR", *coordinator, 1)
	if *sshPub != "" {
		key := strings.TrimSpace(readFile(*sshPub))
		cloudConfig += fmt.Sprintf("\nssh_authorized_keys:\n    - %s\n", key)
	}
	if os.Getenv("USER") == "bradfitz" {
		cloudConfig += fmt.Sprintf("\nssh_authorized_keys:\n    - %s\n", "ssh-rsa AAAAB3NzaC1yc2EAAAABIwAAAIEAwks9dwWKlRC+73gRbvYtVg0vdCwDSuIlyt4z6xa/YU/jTDynM4R4W10hm2tPjy8iR1k8XhDv4/qdxe6m07NjG/By1tkmGpm1mGwho4Pr5kbAAy/Qg+NLCSdAYnnE00FQEcFOC15GFVMOW2AzDGKisReohwH9eIzHPzdYQNPRWXE= bradfitz@papag.bradfitz.com")
	}
	const maxCloudConfig = 32 << 10 // per compute API docs
	if len(cloudConfig) > maxCloudConfig {
		log.Fatalf("cloud config length of %d bytes is over %d byte limit", len(cloudConfig), maxCloudConfig)
	}

	instance := &compute.Instance{
		Name:        *instName,
		Description: "Go Builder",
		MachineType: machType,
		Disks:       []*compute.AttachedDisk{instanceDisk(computeService)},
		Tags: &compute.Tags{
			Items: []string{"http-server", "https-server", "allow-ssh"},
		},
		Metadata: &compute.Metadata{
			Items: []*compute.MetadataItems{
				{
					Key:   "user-data",
					Value: googleapi.String(cloudConfig),
				},
			},
		},
		NetworkInterfaces: []*compute.NetworkInterface{
			&compute.NetworkInterface{
				AccessConfigs: []*compute.AccessConfig{
					&compute.AccessConfig{
						Type:  "ONE_TO_ONE_NAT",
						Name:  "External NAT",
						NatIP: natIP,
					},
				},
				Network: prefix + "/global/networks/default",
			},
		},
		ServiceAccounts: []*compute.ServiceAccount{
			{
				Email: "default",
				Scopes: []string{
					compute.DevstorageFullControlScope,
					compute.ComputeScope,
					compute.CloudPlatformScope,
				},
			},
		},
	}

	log.Printf("Creating instance...")
	op, err := computeService.Instances.Insert(*proj, *zone, instance).Do()
	if err != nil {
		log.Fatalf("Failed to create instance: %v", err)
	}
	opName := op.Name
	log.Printf("Created. Waiting on operation %v", opName)
OpLoop:
	for {
		time.Sleep(2 * time.Second)
		op, err := computeService.ZoneOperations.Get(*proj, *zone, opName).Do()
		if err != nil {
			log.Fatalf("Failed to get op %s: %v", opName, err)
		}
		switch op.Status {
		case "PENDING", "RUNNING":
			log.Printf("Waiting on operation %v", opName)
			continue
		case "DONE":
			if op.Error != nil {
				for _, operr := range op.Error.Errors {
					log.Printf("Error: %+v", operr)
				}
				log.Fatalf("Failed to start.")
			}
			log.Printf("Success. %+v", op)
			break OpLoop
		default:
			log.Fatalf("Unknown status %q: %+v", op.Status, op)
		}
	}

	inst, err := computeService.Instances.Get(*proj, *zone, *instName).Do()
	if err != nil {
		log.Fatalf("Error getting instance after creation: %v", err)
	}
	ij, _ := json.MarshalIndent(inst, "", "    ")
	log.Printf("Instance: %s", ij)
}

func tokenSource() oauth2.TokenSource {
	var tokensource oauth2.TokenSource
	tokenSource, err := google.DefaultTokenSource(oauth2.NoContext)
	if err == nil {
		return tokenSource
	}
	oauthConfig := &oauth2.Config{
		// The client-id and secret should be for an "Installed Application" when using
		// the CLI. Later we'll use a web application with a callback.
		ClientID:     readFile(stagingPrefix() + "client-id.dat"),
		ClientSecret: readFile(stagingPrefix() + "client-secret.dat"),
		Endpoint:     google.Endpoint,
		Scopes: []string{
			compute.DevstorageFullControlScope,
			compute.ComputeScope,
			compute.CloudPlatformScope,
			"https://www.googleapis.com/auth/sqlservice",
			"https://www.googleapis.com/auth/sqlservice.admin",
		},
		RedirectURL: "urn:ietf:wg:oauth:2.0:oob",
	}
	tokenFileName := stagingPrefix() + "token.dat"
	tokenFile := tokenCacheFile(tokenFileName)
	tokenSource = oauth2.ReuseTokenSource(nil, tokenFile)
	token, err := tokenSource.Token()
	if err != nil {
		log.Printf("Error getting token from %s: %v", tokenFileName, err)
		log.Printf("Get auth code from %v", oauthConfig.AuthCodeURL("my-state"))
		fmt.Print("\nEnter auth code: ")
		sc := bufio.NewScanner(os.Stdin)
		sc.Scan()
		authCode := strings.TrimSpace(sc.Text())
		token, err = oauthConfig.Exchange(oauth2.NoContext, authCode)
		if err != nil {
			log.Fatalf("Error exchanging auth code for a token: %v", err)
		}
		if err := tokenFile.WriteToken(token); err != nil {
			log.Fatalf("Error writing to %s: %v", tokenFileName, err)
		}
		tokenSource = oauth2.ReuseTokenSource(token, nil)
	}
	return tokensource
}

func instanceDisk(svc *compute.Service) *compute.AttachedDisk {
	const imageURL = "https://www.googleapis.com/compute/v1/projects/coreos-cloud/global/images/coreos-stable-723-3-0-v20150804"
	diskName := *instName + "-coreos-stateless-pd"

	if *reuseDisk {
		dl, err := svc.Disks.List(*proj, *zone).Do()
		if err != nil {
			log.Fatalf("Error listing disks: %v", err)
		}
		for _, disk := range dl.Items {
			if disk.Name != diskName {
				continue
			}
			return &compute.AttachedDisk{
				AutoDelete: false,
				Boot:       true,
				DeviceName: diskName,
				Type:       "PERSISTENT",
				Source:     disk.SelfLink,
				Mode:       "READ_WRITE",

				// The GCP web UI's "Show REST API" link includes a
				// "zone" parameter, but it's not in the API
				// description. But it wants this form (disk.Zone, a
				// full zone URL, not *zone):
				// Zone: disk.Zone,
				// ... but it seems to work without it.  Keep this
				// comment here until I file a bug with the GCP
				// people.
			}
		}
	}

	diskType := ""
	if *ssd {
		diskType = "https://www.googleapis.com/compute/v1/projects/" + *proj + "/zones/" + *zone + "/diskTypes/pd-ssd"
	}

	return &compute.AttachedDisk{
		AutoDelete: !*reuseDisk,
		Boot:       true,
		Type:       "PERSISTENT",
		InitializeParams: &compute.AttachedDiskInitializeParams{
			DiskName:    diskName,
			SourceImage: imageURL,
			DiskSizeGb:  50,
			DiskType:    diskType,
		},
	}
}

type tokenCacheFile string

func (f tokenCacheFile) Token() (*oauth2.Token, error) {
	slurp, err := ioutil.ReadFile(string(f))
	if err != nil {
		return nil, err
	}
	t := new(oauth2.Token)
	if err := json.Unmarshal(slurp, t); err != nil {
		return nil, err
	}
	return t, nil
}

func (f tokenCacheFile) WriteToken(t *oauth2.Token) error {
	jt, err := json.Marshal(t)
	if err != nil {
		return err
	}
	return ioutil.WriteFile(string(f), jt, 0600)
}
