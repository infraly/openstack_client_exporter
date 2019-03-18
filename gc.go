package main

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/gophercloud/gophercloud"
	"github.com/gophercloud/gophercloud/openstack"
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
			if strings.HasPrefix(server.Name, resourceTag) {
				if time.Since(server.Created) > garbageCollectorResourceAge {
					err = servers.Delete(computeClient, server.ID).ExtractErr()

					if err == nil {
						log.Printf("gc: server %s deleted\n", server.Name)
					} else {
						log.Printf("gc: server deletion failed: %s\n", err)
					}
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

		for _, secGroup := range securityGroups {
			// We had to define our own type because groups.SecGroup doesn't contain Created and Updated
			var s struct {
				SecurityGroup *struct {
					ID      string
					Name    string
					Created time.Time `json:"created_at"`
				} `json:"security_group"`
			}

			if err := groups.Get(networkClient, secGroup.ID).ExtractInto(&s); err != nil {
				return false, fmt.Errorf("gc: failed to get security group details: %s", err)
			}

			securityGroup := s.SecurityGroup

			if strings.HasPrefix(securityGroup.Name, resourceTag) {
				if time.Since(securityGroup.Created) > garbageCollectorResourceAge {
					err = groups.Delete(networkClient, securityGroup.ID).ExtractErr()

					if err == nil {
						log.Printf("gc: security group %s deleted\n", securityGroup.Name)
					} else if !strings.Contains(err.Error(), "SecurityGroupInUse") {
						log.Printf("gc: security group deletion failed: %s\n", err)
					}
				}
			}
		}

		return true, nil
	})

	if err != nil {
		log.Printf("Failed to list left over security groups: %s\n", err)
	}

	// TODO: SSH keys

	if err := gcFloatingIPs(provider); err != nil {
		log.Printf("floating ip garbage collection failure: %s", err)
	}

	if err := gcObjectStorage(provider); err != nil {
		log.Printf("object store garbage collection failure: %s", err)
	}

	return nil
}

func parseTimestampHeader(r containers.GetResult) (time.Time, error) {
	hdr := r.Header.Get("X-Timestamp")

	parts := strings.Split(hdr, ".")

	if len(parts) != 2 {
		return time.Unix(0, 0), fmt.Errorf("X-Timestamp header is not formatted as expected: %s", hdr)
	}

	seconds, err := strconv.ParseInt(parts[0], 10, 64)

	if err != nil {
		return time.Unix(0, 0), err
	}

	t := time.Unix(seconds, 0)

	return t, nil
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
			err := objects.List(objectClient, containerName, objects.ListOpts{Full: true}).EachPage(func(page pagination.Page) (bool, error) {
				objectList, err := objects.ExtractInfo(page)

				if err != nil {
					log.Printf("gc: failed to parse objects: %s", err)
				}

				for _, object := range objectList {
					if time.Since(object.LastModified) > garbageCollectorResourceAge {
						if _, err := objects.Delete(objectClient, containerName, object.Name, objects.DeleteOpts{}).Extract(); err != nil {
							log.Printf("gc: object %s deletion failed: %s", object.Name, err)
						} else {
							log.Printf("gc: object %s deleted from container %s", object.Name, containerName)
						}
					}
				}

				return true, nil
			})

			if err != nil {
				log.Printf("gc: failed to list objects: %s", err)
			}

			result := containers.Get(objectClient, containerName, containers.GetOpts{})

			lastModified, err := parseTimestampHeader(result)

			if err != nil {
				log.Printf("gc: unable to parse X-Timestamp header: %s", err)
				continue
			}

			objectCount, err := strconv.Atoi(result.Header.Get("X-Container-Object-Count"))

			if err != nil {
				log.Printf("gc: unable to parse X-Container-Object-Count: %s", err)
				continue
			}

			if objectCount == 0 && time.Since(lastModified) > garbageCollectorResourceAge {
				if _, err := containers.Delete(objectClient, containerName).Extract(); err != nil {
					log.Printf("gc: failed to delete container %s: %s", containerName, err)
				} else {
					log.Printf("gc: container %s deleted", containerName)
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
			if strings.HasPrefix(fip.Description, resourceTag) {
				var floatingIP struct {
					ID          string    `json:"id"`
					IP          string    `json:"floating_ip_address"`
					Description string    `json:"description"`
					UpdatedAt   time.Time `json:"updated_at"`
				}

				result := floatingips.Get(networkClient, fip.ID)

				// log.Printf("gc: result: %v", result)

				if err := result.ExtractInto(&floatingIP); err != nil {
					log.Printf("gc: cannot extract floating ip details: %v", err)
				}

				// log.Printf("gc: parsed result: %v", floatingIP)

				if !floatingIP.UpdatedAt.IsZero() && time.Since(floatingIP.UpdatedAt) > garbageCollectorResourceAge {
					if err := floatingips.Delete(networkClient, floatingIP.ID).ExtractErr(); err != nil {
						log.Printf("gc: floating ip %v deletion failed: %s", floatingIP.IP, err)
					} else {
						log.Printf("gc: floating ip %v deleted", floatingIP.IP)
					}
				}
			}
		}

		return true, nil
	}); err != nil {
		return fmt.Errorf("gc: failed to list floating ips: %v", err)
	}

	return nil
}
