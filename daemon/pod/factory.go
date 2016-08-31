package pod

import (
	"io"

	"github.com/docker/docker/daemon/logger"
	"github.com/docker/docker/pkg/version"
	dockertypes "github.com/docker/engine-api/types"

	apitypes "github.com/hyperhq/hyperd/types"
	"github.com/hyperhq/hyperd/utils"
	runv "github.com/hyperhq/runv/api"
)

type ContainerEngine interface {
	ContainerCreate(params dockertypes.ContainerCreateConfig) (dockertypes.ContainerCreateResponse, error)

	ContainerInspect(id string, size bool, version version.Version) (interface{}, error)

	ContainerRm(name string, config *dockertypes.ContainerRmConfig)
}

type PodStorage interface {
	Type() string

	PrepareContainer(mountId, sharedDir string) (*runv.VolumeDescription, error)
	InjectFile(src io.Reader, containerId, target, baseDir string, perm, uid, gid int) error
	CreateVolume(podId string, spec *apitypes.UserVolume) error
}

type GlobalLogConfig struct {
	*apitypes.PodLogConfig
	PathPrefix  string
	PodIdInPath bool
}

type PodFactory struct {
	sd         PodStorage
	registry   *PodList
	engine     ContainerEngine
	hosts      *utils.Initializer
	logCfg     *GlobalLogConfig
	logCreator logger.Creator
}

func NewPodFactory(sd PodStorage, eng ContainerEngine, logCfg *GlobalLogConfig) *PodFactory {
	return &PodFactory{
		sd: sd,
		engine: eng,
		hosts: nil,
		logCfg: logCfg,
	}
}
