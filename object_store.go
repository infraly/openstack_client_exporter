package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"

	"github.com/gophercloud/gophercloud"
	"github.com/gophercloud/gophercloud/acceptance/tools"
	"github.com/gophercloud/gophercloud/openstack"
	"github.com/gophercloud/gophercloud/openstack/objectstorage/v1/containers"
	"github.com/gophercloud/gophercloud/openstack/objectstorage/v1/objects"
	"github.com/prometheus/client_golang/prometheus"
)

const fileSize int64 = 100 << (10 * 2)

func objectStoreMain(ctx context.Context, registry *prometheus.Registry) {
	success := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: program + "_object_store",
		Name:      "success",
		Help:      "'1' when a file was successfuly uploaded and downloaded from the object store",
	},
		[]string{"error"},
	)

	timing := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: program + "_object_store",
		Name:      "timing",
		Help:      "Timestamp of each step for uploading and downloadin a file from the object store",
	},
		[]string{
			"step",
		},
	)

	registry.MustRegister(success)
	registry.MustRegister(timing)

	c1 := make(chan error, 1)
	go func() {
		c1 <- uploadDownloadFile(ctx, *timing)
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

type zeroes struct {
	Offset int64
	Length int64
}

func (z *zeroes) Read(b []byte) (n int, err error) {
	// log.Printf("Read: len(b)=%v, z.pos=%v", len(b), z.Offset)

	if z.Offset == z.Length {
		return 0, io.EOF
	}

	written := 0
	for i := range b {
		b[i] = 0
		z.Offset++
		written++

		if z.Offset == z.Length {
			// log.Printf("return %v, %v", written, io.EOF)
			return written, io.EOF
		}
	}

	// log.Printf("return %v, %v", written, nil)
	return written, nil
}

func (z *zeroes) Seek(offset int64, whence int) (int64, error) {
	// log.Printf("Seek: offset=%d, whence=%d\n", offset, whence)

	z.Offset = offset
	return z.Offset, nil
}

func uploadDownloadFile(ctx context.Context, timing prometheus.GaugeVec) error {
	if err := step(ctx, timing, "start"); err != nil {
		return err
	}

	resourceName := tools.RandomString(resourceTag+"-", 8)
	log.Printf("Using random resource name %s\n", resourceName)

	provider, err := getProvider()

	if err != nil {
		return err
	}

	if err := step(ctx, timing, "auth_ok"); err != nil {
		return err
	}

	client, err := openstack.NewObjectStorageV1(provider, gophercloud.EndpointOpts{})

	if err != nil {
		return fmt.Errorf("object store client failure: %s", err)
	}

	// Create a container

	if _, err := containers.Create(client, resourceName, containers.CreateOpts{}).Extract(); err != nil {
		return fmt.Errorf("failed to create container: %s", err)
	}

	if err := step(ctx, timing, "container_created"); err != nil {
		return err
	}

	// Upload a file into our new containe
	objectOpts := objects.CreateOpts{
		Content: &zeroes{0, fileSize},
	}

	if _, err := objects.Create(client, resourceName, resourceName, objectOpts).Extract(); err != nil {
		return fmt.Errorf("failed to upload object: %s", err)
	}

	if err := step(ctx, timing, "object_uploaded"); err != nil {
		return err
	}

	// Download our file back

	downloadResult := objects.Download(client, resourceName, resourceName, objects.DownloadOpts{})

	if _, err := downloadResult.Extract(); err != nil {
		return fmt.Errorf("download failed: %s", err)
	}

	defer downloadResult.Body.Close()

	devNull, err := os.Create(os.DevNull)

	if err != nil {
		return fmt.Errorf("cannot open /dev/null: %s", err)
	}

	defer devNull.Close()

	// TODO: check file content

	written, err := io.Copy(devNull, downloadResult.Body)

	if err != nil {
		return fmt.Errorf("failed to download object: %s", err)
	}

	log.Printf("%v bytes written", written)

	if err := step(ctx, timing, "object_downloaded"); err != nil {
		return err
	}

	// Delete object

	if _, err := objects.Delete(client, resourceName, resourceName, objects.DeleteOpts{}).Extract(); err != nil {
		return fmt.Errorf("failed to delete object")
	}

	if err := step(ctx, timing, "object_deleted"); err != nil {
		return err
	}

	// Delete container

	if _, err := containers.Delete(client, resourceName).Extract(); err != nil {
		return fmt.Errorf("failed to delete container: %s", err)
	}

	if err := step(ctx, timing, "container_deleted"); err != nil {
		return err
	}

	if err := step(ctx, timing, "end"); err != nil {
		return err
	}

	return nil
}
