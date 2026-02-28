package coroot_servicemap

import (
	"flashcat.cloud/categraf/config"
	"flashcat.cloud/categraf/inputs"
)

const inputName = "coroot_servicemap"

type ServiceMapPlugin struct {
	config.PluginConfig
	Instances []*Instance `toml:"instances"`
}

func init() {
	inputs.Add(inputName, func() inputs.Input {
		return &ServiceMapPlugin{}
	})
}

func (p *ServiceMapPlugin) Clone() inputs.Input {
	return &ServiceMapPlugin{}
}

func (p *ServiceMapPlugin) Name() string {
	return inputName
}

func (p *ServiceMapPlugin) GetInstances() []inputs.Instance {
	ret := make([]inputs.Instance, len(p.Instances))
	for i := 0; i < len(p.Instances); i++ {
		ret[i] = p.Instances[i]
	}
	return ret
}

func (p *ServiceMapPlugin) Drop() {
	for _, ins := range p.Instances {
		ins.Drop()
	}
}
