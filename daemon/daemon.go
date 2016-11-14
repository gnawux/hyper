package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"

	"github.com/hyperhq/hyperd/daemon/daemondb"
	"github.com/hyperhq/hyperd/daemon/pod"
	apitypes "github.com/hyperhq/hyperd/types"

	docker "github.com/docker/docker/daemon"
	"github.com/docker/docker/daemon/logger/jsonfilelog"
	"github.com/docker/docker/opts"
	flag "github.com/docker/docker/pkg/mflag"
	"github.com/docker/docker/registry"
	dockerutils "github.com/docker/docker/utils"
	"github.com/golang/glog"
	"github.com/hyperhq/hyperd/utils"
	"github.com/hyperhq/runv/factory"
)

var (
	DefaultLogPrefix string = "/var/run/hyper/Pods"
)

type Daemon struct {
	*docker.Daemon
	ID          string
	db          *daemondb.DaemonDB
	PodList     *pod.PodList
	Factory     factory.Factory
	Kernel      string
	Initrd      string
	Bios        string
	Cbfs        string
	VboxImage   string
	BridgeIface string
	BridgeIP    string
	Host        string
	Storage     Storage
	Hypervisor  string
	DefaultLog  *pod.GlobalLogConfig
}

func (daemon *Daemon) Restore() error {
	if daemon.GetPodNum() == 0 {
		return nil
	}

	ch := daemon.db.GetAllPods()
	if ch == nil {
		estr := "Cannot list pods in leveldb"
		glog.Error(estr)
		return errors.New(estr)
	}

	for {
		item, ok := <-ch
		if !ok {
			break
		}
		if item == nil {
			estr := "error during load pods from leveldb"
			glog.Error(estr)
			return errors.New(estr)
		}

		podId := string(item.K[4:])

		glog.V(1).Infof("reloading pod %s with args %s", podId, string(item.V))

		daemon.db.DeletePod(podId)

		var podSpec apitypes.UserPod
		err := json.Unmarshal(item.V, &podSpec)
		if err != nil {
			return err
		}

		vmId, err := daemon.db.GetP2V(podId)
		if err != nil {
			glog.V(1).Infof("no existing VM for pod %s: %v", podId, err)
		}

		fc := pod.NewPodFactory(daemon.Factory, daemon.Storage, daemon.Daemon, daemon.DefaultLog)

		p, err := pod.LoadXPod(fc, podSpec, vmId)
		if err != nil {
			glog.Warningf("Got a unexpected error when creating(load) pod %s, %v", podId, err)
			continue
		}

		if glog.V(3) {
			p.Log(pod.TRACE, "containers in pod %s: %v", p.Id(), p.ContainerIds())
		}
	}

	return nil
}

func NewDaemon(cfg *apitypes.HyperConfig) (*Daemon, error) {
	daemon, err := NewDaemonFromDirectory(cfg)
	if err != nil {
		return nil, err
	}
	return daemon, nil
}

func NewDaemonFromDirectory(cfg *apitypes.HyperConfig) (*Daemon, error) {
	var tempdir = path.Join(utils.HYPER_ROOT, "run")
	os.Setenv("TMPDIR", tempdir)
	if err := os.MkdirAll(tempdir, 0755); err != nil && !os.IsExist(err) {
		return nil, err
	}

	var realRoot = path.Join(utils.HYPER_ROOT, "lib")
	// Create the root directory if it doesn't exists
	if err := os.MkdirAll(realRoot, 0755); err != nil && !os.IsExist(err) {
		return nil, err
	}

	var (
		db_file = fmt.Sprintf("%s/hyper.db", realRoot)
	)
	db, err := daemondb.NewDaemonDB(db_file)
	if err != nil {
		return nil, err
	}

	daemon := &Daemon{
		ID:          fmt.Sprintf("%d", os.Getpid()),
		db:          db,
		Kernel:      cfg.Kernel,
		Initrd:      cfg.Initrd,
		Bios:        "",
		Cbfs:        "",
		VboxImage:   "",
		PodList:     pod.NewPodList(),
		Host:        cfg.Host,
		BridgeIP:    cfg.BridgeIP,
		BridgeIface: cfg.Bridge,
	}

	daemon.Daemon, err = docker.NewDaemon(dockerCfg, registryCfg)
	if err != nil {
		return nil, err
	}

	// Get the docker daemon info
	sysinfo, err := daemon.Daemon.SystemInfo()
	if err != nil {
		return nil, err
	}
	stor, err := StorageFactory(sysinfo, daemon.db)
	if err != nil {
		return nil, err
	}
	daemon.Storage = stor
	daemon.Storage.Init()

	return daemon, nil
}

var (
	dockerCfg   = &docker.Config{}
	registryCfg = &registry.Service{}
)

func presentInHelp(usage string) string { return usage }
func absentFromHelp(string) string      { return "" }

func InitDockerCfg(mirrors []string, insecureRegistries []string, graphdriver, root string) {
	if dockerCfg.LogConfig.Config == nil {
		dockerCfg.LogConfig.Config = make(map[string]string)
	}

	dockerCfg.LogConfig.Config = make(map[string]string)
	var errhandler flag.ErrorHandling = flag.ContinueOnError
	flags := flag.NewFlagSet("", errhandler)
	dockerCfg.InstallFlags(flags, presentInHelp)

	dockerCfg.GraphDriver = graphdriver
	dockerCfg.Root = root
	dockerCfg.TrustKeyPath = path.Join(root, "keys")

	// disable docker network
	flags.Set("-bridge", "none")
	flags.Set("-iptables", "false")
	flags.Set("-ipmasq", "false")

	// disable log driver
	dockerCfg.LogConfig.Type = "none"

	// debug mode
	if glog.V(3) {
		dockerutils.EnableDebug()
	}

	registryOpts := &registry.Options{
		Mirrors:            opts.NewListOpts(nil),
		InsecureRegistries: opts.NewListOpts(nil),
	}
	registryOpts.InstallFlags(flags, absentFromHelp)

	for _, m := range mirrors {
		registryOpts.Mirrors.Set(m)
	}

	for _, ir := range insecureRegistries {
		registryOpts.InsecureRegistries.Set(ir)
	}

	registryCfg = registry.NewService(registryOpts)
}

func (daemon *Daemon) DefaultLogCfg(driver string, cfg map[string]string) {
	if driver == "" {
		driver = jsonfilelog.Name
	}

	var (
		logPath   = DefaultLogPrefix
		podInPath = true
	)

	if driver == jsonfilelog.Name {
		if lp, ok := cfg["PodLogPrefix"]; ok {
			logPath = lp
			delete(cfg, "PodLogPrefix")
		}

		if pip, ok := cfg["PodIdInPath"]; ok {
			pip = strings.ToLower(pip)
			if pip == "" || pip == "false" || pip == "no" || pip == "0" {
				podInPath = false
			}
			delete(cfg, "PodIdInPath")
		}
	}

	daemon.DefaultLog = &pod.GlobalLogConfig{
		PodLogConfig: &apitypes.PodLogConfig{
			Type:   driver,
			Config: cfg,
		},
		PathPrefix:  logPath,
		PodIdInPath: podInPath,
	}
}

func (daemon *Daemon) GetPodNum() int64 {
	pods, err := daemon.db.ListPod()
	if err != nil {
		return 0
	}
	return int64(len(pods))
}

func (daemon *Daemon) DeleteVolumeId(podId string) error {
	vols, err := daemon.db.ListPodVolumes(podId)
	if err != nil {
		return err
	}
	for _, vol := range vols {
		daemon.Storage.RemoveVolume(podId, vol)
	}
	return daemon.db.DeletePodVolumes(podId)
}

func (daemon *Daemon) WritePodAndContainers(podId string) error {
	p, ok := daemon.PodList.Get(podId)
	if !ok {
		return fmt.Errorf("Cannot find Pod %s to write", podId)
	}

	containers := []string{}
	for _, c := range p.ContainerIds() {
		containers = append(containers, c)
	}

	return daemon.db.UpdateP2C(podId, containers)
}

func (daemon *Daemon) GetVmByPodId(podId string) (string, error) {
	p, ok := daemon.PodList.Get(podId)
	if !ok {
		return "", fmt.Errorf("Not found Pod %s", podId)
	}
	return p.SandboxName(), nil
}

func (daemon *Daemon) GetPodByContainerIdOrName(name string) (*pod.XPod, error) {
	if p, _, ok := daemon.PodList.GetByContainerIdOrName(name); ok {
		return p, nil
	} else {
		return nil, fmt.Errorf("cannot find container %s", name)
	}
}

func (daemon *Daemon) DestroyAllVm() error {
	var remains = []*pod.XPod{}
	daemon.PodList.Foreach(func(p *pod.XPod) error {
		remains = append(remains, p)
		return nil
	})
	for _, p := range remains {
		if err := p.Stop(5); err != nil {
			glog.V(1).Infof("fail to stop %s: %v", p.Id(), err)
		}
	}
	return nil
}

func (daemon *Daemon) DestroyAndKeepVm() error {
	err := daemon.ReleaseAllVms()
	if err != nil {
		return err
	}
	return nil
}

func (daemon *Daemon) Shutdown() error {
	glog.V(0).Info("The daemon will be shutdown")
	glog.V(0).Info("Shutdown all VMs")

	daemon.Factory.CloseFactory()
	daemon.db.Close()
	glog.Flush()
	return nil
}
