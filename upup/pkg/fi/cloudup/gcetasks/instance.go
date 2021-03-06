package gcetasks

import (
	"fmt"

	"github.com/golang/glog"
	"google.golang.org/api/compute/v1"
	"k8s.io/kops/upup/pkg/fi"
	"k8s.io/kops/upup/pkg/fi/cloudup/gce"
	"k8s.io/kops/upup/pkg/fi/cloudup/terraform"
	"reflect"
	"strings"
	"time"
)

var scopeAliases map[string]string

//go:generate fitask -type=Instance
type Instance struct {
	Name        *string
	Network     *Network
	Tags        []string
	Preemptible *bool
	Image       *string
	Disks       map[string]*PersistentDisk

	CanIPForward *bool
	IPAddress    *IPAddress
	Subnet       *Subnet

	Scopes []string

	Metadata    map[string]fi.Resource
	Zone        *string
	MachineType *string

	metadataFingerprint string
}

var _ fi.CompareWithID = &Instance{}

func (e *Instance) CompareWithID() *string {
	return e.Name
}

func (e *Instance) Find(c *fi.Context) (*Instance, error) {
	cloud := c.Cloud.(*gce.GCECloud)

	r, err := cloud.Compute.Instances.Get(cloud.Project, *e.Zone, *e.Name).Do()
	if err != nil {
		if gce.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("error listing Instances: %v", err)
	}

	actual := &Instance{}
	actual.Name = &r.Name
	for _, tag := range r.Tags.Items {
		actual.Tags = append(actual.Tags, tag)
	}
	actual.Zone = fi.String(lastComponent(r.Zone))
	actual.MachineType = fi.String(lastComponent(r.MachineType))
	actual.CanIPForward = &r.CanIpForward

	if r.Scheduling != nil {
		actual.Preemptible = &r.Scheduling.Preemptible
	}
	if len(r.NetworkInterfaces) != 0 {
		ni := r.NetworkInterfaces[0]
		actual.Network = &Network{Name: fi.String(lastComponent(ni.Network))}
		if len(ni.AccessConfigs) != 0 {
			ac := ni.AccessConfigs[0]
			if ac.NatIP != "" {
				addr, err := cloud.Compute.Addresses.List(cloud.Project, cloud.Region).Filter("address eq " + ac.NatIP).Do()
				if err != nil {
					return nil, fmt.Errorf("error querying for address %q: %v", ac.NatIP, err)
				} else if len(addr.Items) != 0 {
					actual.IPAddress = &IPAddress{Name: &addr.Items[0].Name}
				} else {
					return nil, fmt.Errorf("address not found %q: %v", ac.NatIP, err)
				}
			}
		}
	}

	for _, serviceAccount := range r.ServiceAccounts {
		for _, scope := range serviceAccount.Scopes {
			actual.Scopes = append(actual.Scopes, scopeToShortForm(scope))
		}
	}

	actual.Disks = make(map[string]*PersistentDisk)
	for i, disk := range r.Disks {
		if i == 0 {
			source := disk.Source

			// TODO: Parse source URL instead of assuming same project/zone?
			name := lastComponent(source)
			d, err := cloud.Compute.Disks.Get(cloud.Project, *e.Zone, name).Do()
			if err != nil {
				if gce.IsNotFound(err) {
					return nil, fmt.Errorf("disk not found %q: %v", source, err)
				}
				return nil, fmt.Errorf("error querying for disk %q: %v", source, err)
			}

			image, err := ShortenImageURL(cloud.Project, d.SourceImage)
			if err != nil {
				return nil, fmt.Errorf("error parsing source image URL: %v", err)
			}
			actual.Image = fi.String(image)
		} else {
			url, err := gce.ParseGoogleCloudURL(disk.Source)
			if err != nil {
				return nil, fmt.Errorf("unable to parse disk source URL: %q", disk.Source)
			}

			actual.Disks[disk.DeviceName] = &PersistentDisk{Name: &url.Name}
		}
	}

	if r.Metadata != nil {
		actual.Metadata = make(map[string]fi.Resource)
		for _, i := range r.Metadata.Items {
			if i.Value == nil {
				glog.Warningf("ignoring GCE instance metadata entry with nil-value: %q", i.Key)
				continue
			}
			actual.Metadata[i.Key] = fi.NewStringResource(*i.Value)
		}
		actual.metadataFingerprint = r.Metadata.Fingerprint
	}

	return actual, nil
}

func (e *Instance) Run(c *fi.Context) error {
	return fi.DefaultDeltaRunMethod(e, c)
}

func (_ *Instance) CheckChanges(a, e, changes *Instance) error {
	return nil
}

func expandScopeAlias(s string) string {
	switch s {
	case "storage-ro":
		s = "https://www.googleapis.com/auth/devstorage.read_only"
	case "storage-rw":
		s = "https://www.googleapis.com/auth/devstorage.read_write"
	case "compute-ro":
		s = "https://www.googleapis.com/auth/compute.read_only"
	case "compute-rw":
		s = "https://www.googleapis.com/auth/compute"
	case "monitoring":
		s = "https://www.googleapis.com/auth/monitoring"
	case "monitoring-write":
		s = "https://www.googleapis.com/auth/monitoring.write"
	case "logging-write":
		s = "https://www.googleapis.com/auth/logging.write"
	}
	return s
}

func init() {
	scopeAliases = map[string]string{
		"storage-ro":       "https://www.googleapis.com/auth/devstorage.read_only",
		"storage-rw":       "https://www.googleapis.com/auth/devstorage.read_write",
		"compute-ro":       "https://www.googleapis.com/auth/compute.read_only",
		"compute-rw":       "https://www.googleapis.com/auth/compute",
		"monitoring":       "https://www.googleapis.com/auth/monitoring",
		"monitoring-write": "https://www.googleapis.com/auth/monitoring.write",
		"logging-write":    "https://www.googleapis.com/auth/logging.write",
	}
}

func scopeToLongForm(s string) string {
	e, found := scopeAliases[s]
	if found {
		return e
	}
	return s
}

func scopeToShortForm(s string) string {
	for k, v := range scopeAliases {
		if v == s {
			return k
		}
	}
	return s
}

func (e *Instance) mapToGCE(project string, ipAddressResolver func(*IPAddress) (*string, error)) (*compute.Instance, error) {
	zone := *e.Zone

	var scheduling *compute.Scheduling
	if fi.BoolValue(e.Preemptible) {
		scheduling = &compute.Scheduling{
			OnHostMaintenance: "TERMINATE",
			Preemptible:       true,
		}
	} else {
		scheduling = &compute.Scheduling{
			AutomaticRestart: true,
			// TODO: Migrate or terminate?
			OnHostMaintenance: "MIGRATE",
			Preemptible:       false,
		}
	}

	var disks []*compute.AttachedDisk
	disks = append(disks, &compute.AttachedDisk{
		InitializeParams: &compute.AttachedDiskInitializeParams{
			SourceImage: BuildImageURL(project, *e.Image),
		},
		Boot:       true,
		DeviceName: "persistent-disks-0",
		Index:      0,
		AutoDelete: true,
		Mode:       "READ_WRITE",
		Type:       "PERSISTENT",
	})

	for name, disk := range e.Disks {
		disks = append(disks, &compute.AttachedDisk{
			Source:     disk.URL(project),
			AutoDelete: false,
			Mode:       "READ_WRITE",
			DeviceName: name,
		})
	}

	var tags *compute.Tags
	if e.Tags != nil {
		tags = &compute.Tags{
			Items: e.Tags,
		}
	}

	var networkInterfaces []*compute.NetworkInterface
	if e.IPAddress != nil {
		addr, err := ipAddressResolver(e.IPAddress)
		if err != nil {
			return nil, fmt.Errorf("unable to resolve IP for instance: %v", err)
		}
		if addr == nil {
			return nil, fmt.Errorf("instance IP address has not yet been created")
		}
		networkInterface := &compute.NetworkInterface{
			AccessConfigs: []*compute.AccessConfig{{
				NatIP: *addr,
				Type:  "ONE_TO_ONE_NAT",
			}},
			Network: e.Network.URL(project),
		}
		if e.Subnet != nil {
			networkInterface.Subnetwork = *e.Subnet.Name
		}
		networkInterfaces = append(networkInterfaces, networkInterface)
	}

	var serviceAccounts []*compute.ServiceAccount
	if e.Scopes != nil {
		var scopes []string
		for _, s := range e.Scopes {
			s = expandScopeAlias(s)

			scopes = append(scopes, s)
		}
		serviceAccounts = append(serviceAccounts, &compute.ServiceAccount{
			Email:  "default",
			Scopes: scopes,
		})
	}

	var metadataItems []*compute.MetadataItems
	for key, r := range e.Metadata {
		v, err := fi.ResourceAsString(r)
		if err != nil {
			return nil, fmt.Errorf("error rendering Instance metadata %q: %v", key, err)
		}
		metadataItems = append(metadataItems, &compute.MetadataItems{
			Key:   key,
			Value: fi.String(v),
		})
	}

	i := &compute.Instance{
		CanIpForward: *e.CanIPForward,

		Disks: disks,

		MachineType: BuildMachineTypeURL(project, zone, *e.MachineType),

		Metadata: &compute.Metadata{
			Items: metadataItems,
		},

		Name: *e.Name,

		NetworkInterfaces: networkInterfaces,

		Scheduling: scheduling,

		ServiceAccounts: serviceAccounts,

		Tags: tags,
	}

	return i, nil
}

func (i *Instance) isZero() bool {
	zero := &Instance{}
	return reflect.DeepEqual(zero, i)
}

func (_ *Instance) RenderGCE(t *gce.GCEAPITarget, a, e, changes *Instance) error {
	cloud := t.Cloud
	project := cloud.Project
	zone := *e.Zone

	ipAddressResolver := func(ip *IPAddress) (*string, error) {
		return ip.Address, nil
	}

	i, err := e.mapToGCE(project, ipAddressResolver)
	if err != nil {
		return err
	}

	if a == nil {
		glog.V(2).Infof("Creating instance %q", i.Name)
		_, err := t.Cloud.Compute.Instances.Insert(project, zone, i).Do()
		if err != nil {
			return fmt.Errorf("error creating Instance: %v", err)
		}
	} else {
		if changes.Metadata != nil {
			glog.V(2).Infof("Updating instance metadata on %q", i.Name)

			i.Metadata.Fingerprint = a.metadataFingerprint

			op, err := cloud.Compute.Instances.SetMetadata(project, zone, i.Name, i.Metadata).Do()
			if err != nil {
				return fmt.Errorf("error setting metadata on instance: %v", err)
			}

			err = waitCompletion(cloud.Compute, project, op)
			if err != nil {
				return fmt.Errorf("error setting metadata on instance: %v", err)
			}

			changes.Metadata = nil
		}

		if !changes.isZero() {
			glog.Errorf("Cannot apply changes to Instance: %v", changes)
			return fmt.Errorf("Cannot apply changes to Instance: %v", changes)
		}
	}

	return nil
}

func waitCompletion(c *compute.Service, project string, op *compute.Operation) error {
	zone := lastComponent(op.Zone)
	var status *compute.Operation
	for {
		var err error
		status, err = c.ZoneOperations.Get(project, zone, op.Name).Do()
		if err != nil {
			return fmt.Errorf("error fetching operation status: %v", err)
		}
		done := false
		switch status.Status {
		case "DONE":
			done = true
		case "PENDING", "RUNNING":
			glog.V(4).Infof("operation status=%v", status.Status)
		}

		if done {
			break
		}

		// TODO: Exponential backoff or similar
		time.Sleep(1 * time.Second)
	}

	if status.Error != nil {
		for _, e := range status.Error.Errors {
			glog.Warningf("operation failed with error: %v", e)
		}

		return fmt.Errorf("operation failed: %v", status.Error.Errors[0].Message)
	}

	if status.Warnings != nil {
		glog.Warningf("operation completed with warnings: %v", status.Warnings)
	}

	return nil
}

func BuildMachineTypeURL(project, zone, name string) string {
	return fmt.Sprintf("https://www.googleapis.com/compute/v1/projects/%s/zones/%s/machineTypes/%s", project, zone, name)
}

func BuildImageURL(defaultProject, nameSpec string) string {
	tokens := strings.Split(nameSpec, "/")
	var project, name string
	if len(tokens) == 2 {
		project = tokens[0]
		name = tokens[1]
	} else if len(tokens) == 1 {
		project = defaultProject
		name = tokens[0]
	} else {
		glog.Exitf("Cannot parse image spec: %q", nameSpec)
	}

	return fmt.Sprintf("https://www.googleapis.com/compute/v1/projects/%s/global/images/%s", project, name)
}

func ShortenImageURL(defaultProject string, imageURL string) (string, error) {
	u, err := gce.ParseGoogleCloudURL(imageURL)
	if err != nil {
		return "", err
	}
	if u.Project == defaultProject {
		return u.Name, nil
	} else {
		return u.Project + "/" + u.Name, nil
	}
}

func (_ *Instance) RenderTerraform(t *terraform.TerraformTarget, a, e, changes *Instance) error {
	project := t.Project

	// This is a "little" hacky...
	ipAddressResolver := func(ip *IPAddress) (*string, error) {
		tf := "${google_compute_address." + *ip.Name + ".address}"
		return &tf, nil
	}

	i, err := e.mapToGCE(project, ipAddressResolver)
	if err != nil {
		return err
	}

	tf := &terraformInstanceTemplate{
		Name:         i.Name,
		CanIPForward: i.CanIpForward,
		MachineType:  lastComponent(i.MachineType),
		Zone:         i.Zone,
		Tags:         i.Tags.Items,
	}

	// TF requires zone
	if tf.Zone == "" && e.Zone != nil {
		tf.Zone = *e.Zone
	}

	tf.AddServiceAccounts(i.ServiceAccounts)

	for _, d := range i.Disks {
		tfd := &terraformAttachedDisk{
			AutoDelete: d.AutoDelete,
			Scratch:    d.Type == "SCRATCH",
			DeviceName: d.DeviceName,

			// TODO: Does this need to be a TF link?
			Disk: lastComponent(d.Source),
		}
		if d.InitializeParams != nil {
			tfd.Disk = d.InitializeParams.DiskName
			tfd.Image = d.InitializeParams.SourceImage
			tfd.Type = d.InitializeParams.DiskType
			tfd.Size = d.InitializeParams.DiskSizeGb
		}
		tf.Disks = append(tf.Disks, tfd)
	}

	tf.AddNetworks(e.Network, e.Subnet, i.NetworkInterfaces)

	tf.AddMetadata(i.Metadata)

	// Using metadata_startup_script is now mandatory (?)
	{
		startupScript, found := tf.Metadata["startup-script"]
		if found {
			delete(tf.Metadata, "startup-script")
		}
		tf.MetadataStartupScript = startupScript
	}

	if i.Scheduling != nil {
		tf.Scheduling = &terraformScheduling{
			AutomaticRestart:  i.Scheduling.AutomaticRestart,
			OnHostMaintenance: i.Scheduling.OnHostMaintenance,
			Preemptible:       i.Scheduling.Preemptible,
		}
	}

	return t.RenderResource("google_compute_instance", i.Name, tf)
}
