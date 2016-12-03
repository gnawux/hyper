package pod

import (
	"fmt"

	"github.com/golang/protobuf/proto"
	"github.com/hyperhq/hyperd/types"
	"github.com/hyperhq/hypercontainer-utils/hlog"
)

/// Layout of Persistent Info of a Pod:
/// PL-{Pod.Id()}: overall layout of the persistent Info of the Pod
///         |`- id/name
///         |`- container id list
///         |`- volume name list
///          `- interface id list
/// SB-{Pod.Id()}: sandbox persistent Info, retrieved from runV
/// PS-{Pod.Id()}: Global Part of Pod Spec
/// PM-{Pod.Id()}: Pod level metadata that could be changed
///         |`- services: service list
///          `- labels
/// CX-{Container.Id()} Container Persistent Info
/// VX-{Pod.ID()}-{Volume.Name()} Volume Persist Info
/// IF-{Pod.ID()}-{Inf.Id()}

const (
	LAYOUT_KEY_PREFIX = "PL-"
	LAYOUT_KEY_FMT = "PL-%s"
	SB_KEY_FMT = "SB-%s"
	PS_KEY_FMT = "PS-%s"
	PM_KEY_FMT = "PM-%s"
	CX_KEY_FMT = "CX-%s"
	VX_KEY_FMT = "VX-%s-%s"
	IF_KEY_FMT = "IF-%s-%s"
)

func (p *XPod) savePod() error {
	var (
		containers = make([]string, 0, len(p.containers))
		volumes = make([]string, 0, len(p.volumes))
		interfaces = make([]string, 0, len(p.interfaces))
	)

	if err := p.saveGlobalSpec(); err != nil {
		return err
	}

	if err := p.savePodMeta(); err != nil {
		return err
	}

	for cid, c := range p.containers {
		containers = append(containers, cid)
		if err := c.saveContainer(); err != nil {
			return err
		}
	}

	for vid, v := range p.volumes {
		volumes = append(volumes, vid)
		if err := v.saveVolume(); err != nil {
			return err
		}
	}

	for inf, i := range  p.interfaces {
		interfaces = append(interfaces, inf)
		if err := i.saveInterface(); err != nil {
			return err
		}
	}

	pl := &types.PersistPodLayout{
		Id: p.Id(),
		Containers: containers,
		Volumes: volumes,
		Interfaces: interfaces,
	}
	return p.saveMessage(fmt.Sprintf(LAYOUT_KEY_FMT, p.Id()), pl, p, "pod layout")
}

func (p *XPod) saveGlobalSpec() error {
	return p.saveMessage(fmt.Sprintf(PS_KEY_FMT, p.Id()), p.globalSpec, p, "global spec")
}

func (p *XPod) savePodMeta() error {
	meta := &types.PersistPodMeta{
		Id: p.Id(),
		Services: p.services,
		Labels: p.labels,
	}
	return p.saveMessage(fmt.Sprintf(PM_KEY_FMT, p.Id()), meta, p, "pod meta")
}

func (c *Container) saveContainer() error {
	cx := &types.PersistContainer{
		Id: c.Id(),
		Pod: c.p.Id(),
		Spec: c.spec,
		Descript: c.descript,
	}
	return c.p.saveMessage(fmt.Sprintf(CX_KEY_FMT, c.Id()), cx, c, "container info")
}

func (v *Volume) saveVolume() error {
	vx := &types.PersistVolume{
		Name: v.spec.Name,
		Pod: v.p.Id(),
		Spec: v.spec,
		Descript: v.descript,
	}
	return v.p.saveMessage(fmt.Sprintf(VX_KEY_FMT, v.p.Id(), v.spec.Name), vx, v, "volume info")
}

func (inf *Interface) saveInterface() error {
	ix := &types.PersistInterface{
		Id: inf.descript.Id,
		Pod: inf.p.Id(),
		Spec: inf.spec,
		Descript: inf.descript,
	}
	return inf.p.saveMessage(fmt.Sprintf(IF_KEY_FMT, inf.p.Id(), inf.descript.Id), ix, inf, "interface info")
}

func (p *XPod) saveMessage(key string, message proto.Message, owner hlog.LogOwner, op string) error {
	pm, err := proto.Marshal(message)
	if err != nil {
		hlog.HLog(ERROR, owner, 2, "failed to serialize %s: %v", op, err)
		return err
	}
	err = p.factory.db.Update([]byte(key),pm)
	if err != nil {
		hlog.HLog(ERROR, owner, 2, "failed to write %s to db: %v", op, err)
		return err
	}
	hlog.HLog(DEBUG, owner, 2, "%s serialized to db", op)
	return nil
}