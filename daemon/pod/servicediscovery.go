package pod

import (
	"path"

	"github.com/hyperhq/hyperd/servicediscovery"
	apitypes "github.com/hyperhq/hyperd/types"
	"github.com/hyperhq/hyperd/utils"
)

func ParseServiceDiscovery(id string, spec *apitypes.UserPod) *apitypes.UserContainer {
	var serviceDir string = path.Join(utils.HYPER_ROOT, "services", id)

	if len(spec.Services) == 0 {
		return nil
	}

	/* PrepareServices will check service volume */
	serviceVolume := &apitypes.UserVolume{
		Name:   "service-volume",
		Source: serviceDir,
		Format: "vfs",
		Fstype: "dir",
	}

	serviceVolRef := &apitypes.UserVolumeReference{
		Volume:   "service-volume",
		Path:     servicediscovery.ServiceVolume,
		ReadOnly: false,
		Detail:   serviceVolume,
	}

	return &apitypes.UserContainer{
		Name:    ServiceDiscoveryContainerName(spec.Id),
		Image:   servicediscovery.ServiceImage,
		Command: []string{"haproxy", "-D", "-f", "/usr/local/etc/haproxy/haproxy.cfg", "-p", "/var/run/haproxy.pid"},
		Volumes: []*apitypes.UserVolumeReference{serviceVolRef},
		Type:    apitypes.UserContainer_SERVICE,
	}
}

func ServiceDiscoveryContainerName(podName string) string {
	return podName + "-service-discovery"
}
