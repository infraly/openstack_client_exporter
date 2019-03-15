package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

	"github.com/gophercloud/gophercloud"
	"github.com/gophercloud/gophercloud/acceptance/tools"
	"github.com/gophercloud/gophercloud/openstack"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/extensions/bootfromvolume"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/extensions/keypairs"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/flavors"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/servers"
	"github.com/gophercloud/gophercloud/openstack/imageservice/v2/images"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/extensions/layer3/floatingips"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/extensions/security/groups"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/extensions/security/rules"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/networks"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/ports"
	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/crypto/ssh"
)

const (
	volumeSize = 10
)

func getImage(client *gophercloud.ServiceClient, name string) (*images.Image, error) {
	page, err := images.List(client, images.ListOpts{Name: name}).AllPages()

	if err != nil {
		return nil, err
	}

	AllImages, err := images.ExtractImages(page)

	if err != nil {
		return nil, err
	}

	if len(AllImages) > 0 {
		return &AllImages[0], nil
	}

	return nil, fmt.Errorf("image not found")
}

func getFlavor(client *gophercloud.ServiceClient, name string) (*flavors.Flavor, error) {
	id, err := flavors.IDFromName(client, name)

	if err != nil {
		return nil, err
	}

	flavor, err := flavors.Get(client, id).Extract()

	if err != nil {
		return nil, err
	}

	return flavor, nil
}

func getNetwork(client *gophercloud.ServiceClient, name string) (*networks.Network, error) {
	page, err := networks.List(client, networks.ListOpts{}).AllPages()

	if err != nil {
		return nil, err
	}

	allNetworks, err := networks.ExtractNetworks(page)

	if err != nil {
		return nil, err
	}

	for _, network := range allNetworks {
		if network.Name == name {
			return &network, nil
		}
	}

	return nil, fmt.Errorf("network not found")
}

func getHostKey(ctx context.Context, client *gophercloud.ServiceClient, server servers.Server, timing prometheus.GaugeVec) (hostKeys []ssh.PublicKey, err error) {
	bootStarted := false
	for {
		consoleOutput, err := servers.ShowConsoleOutput(client, server.ID, servers.ShowConsoleOutputOpts{}).Extract()

		if err == nil {
			if consoleOutput != "" && !bootStarted {
				bootStarted = true
				if err := step(ctx, timing, "boot_started"); err != nil {
					return nil, err
				}
			}
			re := regexp.MustCompile("(?s)-----BEGIN SSH HOST KEY KEYS-----\n(.+)\n-----END SSH HOST KEY KEYS-----")
			match := re.FindStringSubmatch(consoleOutput)

			if len(match) == 2 {
				for _, line := range strings.Split(match[1], "\n") {
					if hostKey, _, _, _, err := ssh.ParseAuthorizedKey([]byte(line)); err != nil {
						log.Printf("failed to parse SSH host key: %s", err)
					} else {
						hostKeys = append(hostKeys, hostKey)
					}
				}

				return hostKeys, nil
			}

			select {
			case <-ctx.Done():
				return nil, fmt.Errorf("timeout while waiting cloud-init ssh host keys")
			default:
			}
		}

		time.Sleep(1 * time.Second)
	}
}

func cleanupServer(computeClient *gophercloud.ServiceClient, server servers.Server) {
	err := servers.Delete(computeClient, server.ID).ExtractErr()

	if err == nil {
		log.Printf("Server %s deleted\n", server.ID)
	} else {
		log.Printf("Server deletion failed: %s\n", err)
	}
}

func cleanupFIP(networkClient *gophercloud.ServiceClient, fip floatingips.FloatingIP) {
	err := floatingips.Delete(networkClient, fip.ID).ExtractErr()

	if err == nil {
		log.Printf("Floating IP %s deleted\n", fip.FloatingIP)
	} else {
		log.Printf("Floating IP deletion failed: %s\n", err)
	}
}

func cleanupSecurityGroup(networkClient *gophercloud.ServiceClient, securityGroup groups.SecGroup) {
	err := groups.Delete(networkClient, securityGroup.ID).ExtractErr()

	if err == nil {
		log.Printf("Security group %s deleted\n", securityGroup.ID)
	} else {
		log.Printf("Security group deletion failed: %s\n", err)
	}
}

func cleanupSSHKeypair(computeClient *gophercloud.ServiceClient, keypair keypairs.KeyPair) {
	err := keypairs.Delete(computeClient, keypair.Name).ExtractErr()

	if err == nil {
		log.Printf("Keypair %s deleted\n", keypair.Name)
	} else {
		log.Printf("Keypair deletion failed: %s\n", err)
	}
}

func getPort(networkClient *gophercloud.ServiceClient, serverID string) (*ports.Port, error) {
	page, err := ports.List(networkClient, ports.ListOpts{DeviceID: serverID}).AllPages()

	if err != nil {
		return nil, fmt.Errorf("failed to get instance port ID")
	}

	allPorts, err := ports.ExtractPorts(page)

	if err != nil {
		return nil, err
	}

	return &allPorts[0], nil
}

func generateSSHKey() (*rsa.PrivateKey, string, error) {
	// Generate private key
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, "", err
	}

	// Generate public key
	publicKey, err := ssh.NewPublicKey(&privateKey.PublicKey)
	if err != nil {
		return nil, "", err
	}

	publicKeyString := string(ssh.MarshalAuthorizedKey(publicKey))

	return privateKey, publicKeyString, nil
}

func sshServer(ctx context.Context, ip string, hostKeys []ssh.PublicKey, privateKey rsa.PrivateKey) error {
	signer, err := ssh.NewSignerFromKey(&privateKey)
	if err != nil {
		return fmt.Errorf("unable to create signer from private key: %s", err)
	}

	config := &ssh.ClientConfig{
		User: "ubuntu",
		Auth: []ssh.AuthMethod{
			ssh.PublicKeys(signer),
		},
		HostKeyCallback: ssh.FixedHostKey(hostKeys[0]), // FIXME: we should probably check against all keys
	}

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout during ssh connection")
		default:
		}

		client, err := ssh.Dial("tcp", ip+":22", config)
		if err != nil {
			log.Printf("Failed to dial: %s", err)
			time.Sleep(1 * time.Second)
			continue
		}
		defer client.Close()

		session, err := client.NewSession()
		if err != nil {
			log.Printf("Failed to create session: %s", err)
			time.Sleep(1 * time.Second)
			continue
		}
		defer session.Close()

		var b bytes.Buffer
		session.Stdout = &b
		if err := session.Run("/usr/bin/whoami"); err != nil {
			log.Printf("Failed to run: " + err.Error())
			time.Sleep(1 * time.Second)
			continue
		}

		log.Printf("SSH connection was successful")

		break
	}

	return nil
}

func spawnInstance(ctx context.Context, timing prometheus.GaugeVec) error {
	if err := step(ctx, timing, "start"); err != nil {
		return err
	}

	resourceName := tools.RandomString(resourceTag+"-", 8)
	log.Printf("Using random resource name %s\n", resourceName)

	provider, err := getProvider(ctx)

	if err != nil {
		return err
	}

	if err := step(ctx, timing, "auth_ok"); err != nil {
		return err
	}

	// Find image ID by name

	imageClient, err := openstack.NewImageServiceV2(provider, gophercloud.EndpointOpts{})

	if err != nil {
		return fmt.Errorf("glance client failure: %s", err)
	}

	image, err := getImage(imageClient, imageName)

	if err != nil {
		return fmt.Errorf("image not found: %s", err)
	}

	log.Printf("Image found %s\n", image.ID)

	if err := step(ctx, timing, "image_id"); err != nil {
		return err
	}

	// Find flavor by name

	computeClient, err := openstack.NewComputeV2(provider, gophercloud.EndpointOpts{})

	if err != nil {
		return fmt.Errorf("nova client failure: %f", err)
	}

	flavor, err := getFlavor(computeClient, flavorName)

	if err != nil {
		return fmt.Errorf("flavor not found: %f", err)
	}

	if err := step(ctx, timing, "flavor_id"); err != nil {
		return err
	}

	// Find internal network by name

	networkClient, err := openstack.NewNetworkV2(provider, gophercloud.EndpointOpts{})

	if err != nil {
		return fmt.Errorf("neutron client failure: %s", err)
	}

	network, err := getNetwork(networkClient, internalNetwork)

	if err != nil {
		return fmt.Errorf("cannot get network: %s", err)
	}

	if err := step(ctx, timing, "network_id"); err != nil {
		return err
	}

	// Create security group

	securityGroup, err := groups.Create(networkClient, groups.CreateOpts{Name: resourceName}).Extract()

	if err != nil {
		return fmt.Errorf("security group failure: %s", err)
	}

	// Neutron tags are not supported on our Mitaka...
	//
	// err = attributestags.Add(networkClient, "security_groups", securityGroup.ID, resourceTag).ExtractErr()

	// if err != nil {
	// 	return fmt.Errorf("security group tagging failed: %s", err)
	// }

	if err := step(ctx, timing, "security_group_created"); err != nil {
		return err
	}

	log.Printf("Security group %s created", securityGroup.ID)

	// Add SSH rule to security group

	createOpts := rules.CreateOpts{
		Direction:    "ingress",
		PortRangeMin: 22,
		EtherType:    rules.EtherType4,
		PortRangeMax: 22,
		Protocol:     "tcp",
		SecGroupID:   securityGroup.ID,
	}

	rule, err := rules.Create(networkClient, createOpts).Extract()

	if err != nil {
		return fmt.Errorf("security group rule failure: %s", err)
	}

	defer cleanupSecurityGroup(networkClient, *securityGroup)

	if err := step(ctx, timing, "security_group_rule_created"); err != nil {
		return err
	}

	log.Printf("Security group rule %s\n", rule.ID)

	// Generate and upload SSH key

	privateKey, publicKey, err := generateSSHKey()

	if err != nil {
		return fmt.Errorf("SSH key creation failure: %s", err)
	}

	keypair, err := keypairs.Create(computeClient, keypairs.CreateOpts{Name: resourceName, PublicKey: publicKey}).Extract()

	if err != nil {
		return fmt.Errorf("SSH key upload failure: %s", err)
	}

	if err := step(ctx, timing, "ssh_key_uploaded"); err != nil {
		return err
	}

	defer cleanupSSHKeypair(computeClient, *keypair)

	// Find external network by name

	externalNetwork, err := getNetwork(networkClient, externalNetwork)

	if err != nil {
		return fmt.Errorf("failed to find external network: %s", err)
	}

	if err := step(ctx, timing, "external_network_id"); err != nil {
		return err
	}

	log.Printf("External network found %s\n", externalNetwork.ID)

	// Create floating IP on the external network

	fip, err := floatingips.Create(networkClient, floatingips.CreateOpts{
		FloatingNetworkID: externalNetwork.ID,
	}).Extract()

	if err != nil {
		return fmt.Errorf("floating IP failure: %s", err)
	}

	defer cleanupFIP(networkClient, *fip)

	if err := step(ctx, timing, "floating_ip_created"); err != nil {
		return err
	}

	log.Printf("Floating IP: %s", fip.FloatingIP)

	// Boot server

	server, err := bootfromvolume.Create(computeClient, bootfromvolume.CreateOptsExt{
		keypairs.CreateOptsExt{
			CreateOptsBuilder: servers.CreateOpts{
				Name:           resourceName,
				FlavorRef:      flavor.ID,
				Networks:       []servers.Network{servers.Network{UUID: network.ID}},
				SecurityGroups: []string{securityGroup.ID},
			},
			KeyName: resourceName,
		},
		[]bootfromvolume.BlockDevice{
			bootfromvolume.BlockDevice{
				BootIndex:           0,
				DeleteOnTermination: true,
				UUID:                image.ID,
				SourceType:          bootfromvolume.SourceImage,
				DestinationType:     bootfromvolume.DestinationVolume,
				VolumeSize:          volumeSize,
			},
		},
	}).Extract()

	if err != nil {
		return fmt.Errorf("server creation failed: %s", err)
	}

	defer cleanupServer(computeClient, *server)

	if err := step(ctx, timing, "server_created"); err != nil {
		return err
	}

	log.Printf("Server created %s\n", server.ID)

	for {
		server, err = servers.Get(computeClient, server.ID).Extract()

		if err == nil && server.Status == "ACTIVE" {
			break
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout waiting for server to reach ACTIVE status")
		default:
		}

		time.Sleep(1 * time.Second)
	}

	if err := step(ctx, timing, "server_active_status"); err != nil {
		return err
	}

	log.Println("Server is ACTIVE")

	// Assign floating IP

	port, err := getPort(networkClient, server.ID)

	if err != nil {
		return fmt.Errorf("cannot get server port: %s", err)
	}

	_, err = floatingips.Update(networkClient, fip.ID, floatingips.UpdateOpts{PortID: &port.ID}).Extract()

	if err != nil {
		return fmt.Errorf("failed to assign floating IP: %s", err)
	}

	if err := step(ctx, timing, "floating_ip_associated"); err != nil {
		return err
	}

	log.Printf("Floating IP %s has been successfuly associated with port %s", fip.FloatingIP, port.ID)

	// Monitor serial console

	for {
		consoleOutput, err := servers.ShowConsoleOutput(computeClient, server.ID, servers.ShowConsoleOutputOpts{}).Extract()

		if err != nil {
			log.Printf("Failed to get console output: %s\n", err)
		} else if len(consoleOutput) > 0 {
			break
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout while waiting for console output")
		default:
		}

		time.Sleep(1 * time.Second)
	}

	var hostKeys []ssh.PublicKey

	hostKeys, err = getHostKey(ctx, computeClient, *server, timing)

	if err != nil {
		log.Printf("host key: %s\n", err)
	}

	if err := step(ctx, timing, "ssh_host_keys_retrieved"); err != nil {
		return err
	}

	log.Printf("Host SSH keys successfuly retrieved")

	// SSH into instance

	if err := sshServer(ctx, fip.FloatingIP, hostKeys, *privateKey); err != nil {
		return fmt.Errorf("SSH connection failed: %s", err)
	}

	if err := step(ctx, timing, "ssh_successful"); err != nil {
		return err
	}

	if err := step(ctx, timing, "end"); err != nil {
		return err
	}

	return nil
}

func spawnMain(ctx context.Context, registry *prometheus.Registry) {
	success := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: program + "_spawn",
		Name:      "success",
		Help:      "'1' when an OpenStack instance was booted from volume and successfully ssh'ed into",
	},
		[]string{"error"},
	)

	timing := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: program + "_spawn",
		Name:      "timing",
		Help:      "Timestamp of each step for booting on OpenStack instance from volume",
	},
		[]string{
			"step",
		},
	)

	registry.MustRegister(success)
	registry.MustRegister(timing)

	c1 := make(chan error, 1)
	go func() {
		c1 <- spawnInstance(ctx, *timing)
	}()

	select {
	case err := <-c1:
		if err != nil {
			log.Printf("ERROR: %s\n", err)
			success.WithLabelValues(fmt.Sprintf("%s", err)).Set(0)
		} else {
			success.WithLabelValues("").Set(1)
		}
	case <-ctx.Done():
		log.Println("ERROR: request timeout reached")
		success.WithLabelValues(fmt.Sprintf("request timeout reached")).Set(0)
	}
}
