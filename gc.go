package main

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gophercloud/gophercloud"
	"github.com/gophercloud/gophercloud/openstack"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/extensions/keypairs"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/servers"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/extensions/layer3/floatingips"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/extensions/security/groups"
	"github.com/gophercloud/gophercloud/openstack/objectstorage/v1/containers"
	"github.com/gophercloud/gophercloud/openstack/objectstorage/v1/objects"
	"github.com/gophercloud/gophercloud/pagination"
)

func runGarbageCollector() {
	for {
		start := time.Now()
		if err := garbageCollector(); err != nil {
			log.Printf("garbage collector error: %s\n", err)
		}

		sleepTime := garbageCollectorSleep - time.Since(start)
		time.Sleep(sleepTime)
	}
}

func shouldDelete(name string) bool {
	if !strings.HasPrefix(name, resourceTag) {
		return false
	}

	re, err := regexp.Compile(resourceTag + "-[0-9a-zA-Z]+-([0-9]+)")

	if err != nil {
		log.Printf("gc: failed to compile re: %s", err)
		return false
	}

	matches := re.FindStringSubmatch(name)

	if len(matches) != 2 {
		log.Printf("gc: failed to match regex on %s: %v", name, matches)
		return false
	}

	ts, err := strconv.ParseInt(matches[1], 10, 64)

	if err != nil {
		log.Printf("gc: cannot parse timestamp from %s", name)
		return false
	}

	timestamp := time.Unix(ts, 0)

	if timestamp.IsZero() {
		log.Printf("gc: zero timestamp for %s", name)
		return false
	}

	return time.Since(timestamp) > garbageCollectorResourceAge
}

func garbageCollector() error {
	// log.Println("gc: starting")
	provider, err := getProvider(context.TODO())

	if err != nil {
		return fmt.Errorf("gc: openstack authentication failure: %f", err)
	}

	computeClient, err := openstack.NewComputeV2(provider, gophercloud.EndpointOpts{})

	if err != nil {
		return fmt.Errorf("gc: nova client failure: %f", err)
	}

	networkClient, err := openstack.NewNetworkV2(provider, gophercloud.EndpointOpts{})

	if err != nil {
		return fmt.Errorf("gc: neutron client failure: %s", err)
	}

	// Servers

	err = servers.List(computeClient, servers.ListOpts{}).EachPage(func(page pagination.Page) (bool, error) {
		serverList, err := servers.ExtractServers(page)
		if err != nil {
			return false, err
		}

		for _, server := range serverList {
			if shouldDelete(server.Name) {
				err = servers.Delete(computeClient, server.ID).ExtractErr()

				if err == nil {
					log.Printf("gc: server %s deleted\n", server.Name)
				} else {
					log.Printf("gc: server deletion failed: %s\n", err)
				}
			}
		}

		return true, nil
	})

	if err != nil {
		log.Printf("Failed to list left over servers: %s\n", err)
	}

	// Security groups

	err = groups.List(networkClient, groups.ListOpts{}).EachPage(func(page pagination.Page) (bool, error) {
		securityGroups, err := groups.ExtractGroups(page)

		if err != nil {
			return false, err
		}

		for _, securityGroup := range securityGroups {
			if shouldDelete(securityGroup.Name) {
				err = groups.Delete(networkClient, securityGroup.ID).ExtractErr()

				if err == nil {
					log.Printf("gc: security group %s deleted\n", securityGroup.Name)
				} else if !strings.Contains(err.Error(), "SecurityGroupInUse") {
					log.Printf("gc: security group %s deletion failed: %s\n", securityGroup.Name, err)
				}
			}
		}

		return true, nil
	})

	if err != nil {
		log.Printf("Failed to list left over security groups: %s\n", err)
	}

	if err := gcKeypairs(provider); err != nil {
		log.Printf("keypair garbage collection failure: %s", err)
	}

	if err := gcFloatingIPs(provider); err != nil {
		log.Printf("floating ip garbage collection failure: %s", err)
	}

	if err := gcObjectStorage(provider); err != nil {
		log.Printf("object store garbage collection failure: %s", err)
	}

	return nil
}

func gcObjectStorage(provider *gophercloud.ProviderClient) error {
	objectClient, err := openstack.NewObjectStorageV1(provider, gophercloud.EndpointOpts{})

	if err != nil {
		return fmt.Errorf("gc: object storage client failure: %s", err)
	}

	if err := containers.List(objectClient, containers.ListOpts{Full: true, Prefix: resourceTag}).EachPage(func(page pagination.Page) (bool, error) {
		containerNames, err := containers.ExtractNames(page)

		if err != nil {
			log.Printf("gc: failed to list containers: %s", err)
		}

		for _, containerName := range containerNames {
			if shouldDelete(containerName) {
				if err := objects.List(objectClient, containerName, objects.ListOpts{Full: true}).EachPage(func(page pagination.Page) (bool, error) {
					objectList, err := objects.ExtractInfo(page)

					if err != nil {
						log.Printf("gc: failed to parse objects: %s", err)
					}

					for _, object := range objectList {
						if _, err := objects.Delete(objectClient, containerName, object.Name, objects.DeleteOpts{}).Extract(); err != nil {
							log.Printf("gc: object %s deletion failed: %s", object.Name, err)
						} else {
							log.Printf("gc: object %s deleted from container %s", object.Name, containerName)
						}

					}

					return true, nil
				}); err != nil {
					log.Printf("gc: failed to list objects: %s", err)
				}

				result := containers.Get(objectClient, containerName, containers.GetOpts{})

				objectCount, err := strconv.Atoi(result.Header.Get("X-Container-Object-Count"))

				if err != nil {
					log.Printf("gc: unable to parse X-Container-Object-Count: %s", err)
					continue
				}

				if objectCount == 0 {
					if _, err := containers.Delete(objectClient, containerName).Extract(); err != nil {
						log.Printf("gc: failed to delete container %s: %s", containerName, err)
					} else {
						log.Printf("gc: container %s deleted", containerName)
					}
				}
			}
		}

		return true, nil
	}); err != nil {
		log.Printf("gc: failed to list containers: %s", err)
	}

	return nil
}

func gcFloatingIPs(provider *gophercloud.ProviderClient) error {
	networkClient, err := openstack.NewNetworkV2(provider, gophercloud.EndpointOpts{})

	if err != nil {
		return fmt.Errorf("gc: network client failure: %s", err)
	}

	if err := floatingips.List(networkClient, floatingips.ListOpts{}).EachPage(func(page pagination.Page) (bool, error) {
		floatingIPs, err := floatingips.ExtractFloatingIPs(page)

		if err != nil {
			log.Printf("gc: cannot extract floating ips from list: %v", err)
		}

		for _, fip := range floatingIPs {
			if shouldDelete(fip.Description) {
				if err := floatingips.Delete(networkClient, fip.ID).ExtractErr(); err != nil {
					log.Printf("gc: floating ip %s deletion failed: %s", fip.FloatingIP, err)
				} else {
					log.Printf("gc: floating ip %s deleted", fip.FloatingIP)
				}
			}
		}

		return true, nil
	}); err != nil {
		return fmt.Errorf("gc: failed to list floating ips: %v", err)
	}

	return nil
}

func gcKeypairs(provider *gophercloud.ProviderClient) error {
	computeClient, err := openstack.NewComputeV2(provider, gophercloud.EndpointOpts{})

	if err != nil {
		return fmt.Errorf("gc: compute client failure: %s", err)
	}

	if err := keypairs.List(computeClient).EachPage(func(page pagination.Page) (bool, error) {
		keyPairs, err := keypairs.ExtractKeyPairs(page)

		if err != nil {
			log.Printf("gc: failed to extract keypairs from page: %s", err)
		}

		for _, keypair := range keyPairs {
			if shouldDelete(keypair.Name) {
				if err := keypairs.Delete(computeClient, keypair.Name).ExtractErr(); err != nil {
					log.Printf("gc: keypair %s deletion failed: %s", keypair.Name, err)
				} else {
					log.Printf("gc: keypair %s deleted", keypair.Name)
				}
			}
		}

		return false, nil
	}); err != nil {
		return fmt.Errorf("gc: failed to list keypairs: %s", err)
	}

	return nil
}
