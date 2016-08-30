package types

func (p *UserPod) LookupContainer(idOrName string) *UserContainer {
	if p == nil {
		return nil
	}
	for _, c := range p.Containers {
		if c.Id == idOrName || c.Name == idOrName {
			return c
		}
	}
	return nil
}
