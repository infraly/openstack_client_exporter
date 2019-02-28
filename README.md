# openstack_client_exporter
A prometheus exporter for monitoring OpenStack from user side

## Usage

```console
$ ./openstack_client_exporter --help
Usage of ./openstack_client_exporter:
  -external-network string
    	name of the external network (default "internet")
  -flavor string
    	name of the instance flavor (default "t2.small")
  -image string
    	name of the image (default "ubuntu-16.04-x86_64")
  -internal-network string
    	name of the internal network (default "private")
```

## Sample output

```console
$ curl localhost:9539/metrics
[ a bunch of standard go metrics ]
# HELP openstack_client_exporter_build_info A metric with a constant '1' value labeled by version, revision, branch, and goversion from which openstack_client_exporter was built.
# TYPE openstack_client_exporter_build_info gauge
openstack_client_exporter_build_info{branch="",goversion="go1.11",revision="",version=""} 1
# HELP openstack_client_object_store_success '1' when a file was successfuly uploaded and downloaded from the object store
# TYPE openstack_client_object_store_success gauge
openstack_client_object_store_success{error="object store client failure: No suitable endpoint could be found in the service catalog."} 0
# HELP openstack_client_object_store_timing Timestamp of each step for uploading and downloadin a file from the object store
# TYPE openstack_client_object_store_timing gauge
openstack_client_object_store_timing{step="auth_ok"} 1.5513639574306479e+09
openstack_client_object_store_timing{step="start"} 1.5513639567950132e+09
# HELP openstack_client_spawn_success '1' when an OpenStack instance was booted from volume and successfully ssh'ed into
# TYPE openstack_client_spawn_success gauge
openstack_client_spawn_success{error=""} 1
# HELP openstack_client_spawn_timing Timestamp of each step for booting on OpenStack instance from volume
# TYPE openstack_client_spawn_timing gauge
openstack_client_spawn_timing{step="auth_ok"} 1.551363957387765e+09
openstack_client_spawn_timing{step="boot_started"} 1.5513639862414603e+09
openstack_client_spawn_timing{step="end"} 1.5513640235120518e+09
openstack_client_spawn_timing{step="external_network_id"} 1.5513639592846642e+09
openstack_client_spawn_timing{step="flavor_id"} 1.5513639577655594e+09
openstack_client_spawn_timing{step="floating_ip_associated"} 1.551363981913244e+09
openstack_client_spawn_timing{step="floating_ip_created"} 1.55136396145901e+09
openstack_client_spawn_timing{step="image_id"} 1.5513639575788805e+09
openstack_client_spawn_timing{step="network_id"} 1.551363958037408e+09
openstack_client_spawn_timing{step="security_group_created"} 1.5513639583344443e+09
openstack_client_spawn_timing{step="security_group_rule_created"} 1.5513639585917253e+09
openstack_client_spawn_timing{step="server_active_status"} 1.5513639798895202e+09
openstack_client_spawn_timing{step="server_created"} 1.5513639632566118e+09
openstack_client_spawn_timing{step="ssh_host_keys_retrieved"} 1.5513640228404794e+09
openstack_client_spawn_timing{step="ssh_key_uploaded"} 1.5513639590000303e+09
openstack_client_spawn_timing{step="ssh_successful"} 1.5513640235120487e+09
openstack_client_spawn_timing{step="start"} 1.5513639567950585e+09
$
```

## Initial setup

It is recommended to create a dedicated OpenStack project for use with this exporter. It needs access to an Ubuntu cloud image with cloud-init

```console
$ wget https://cloud-images.ubuntu.com/xenial/current/xenial-server-cloudimg-amd64-disk1.img
$ openstack image create --file xenial-server-cloudimg-amd64-disk1.img ubuntu-16.04-x86_64
$ openstack network create private
$ openstack subnet create --subnet-range 192.168.0.0/24 --network private private
$ openstack router create router
$ openstack router add subnet router private
$ openstack router set --external-gateway public router
```