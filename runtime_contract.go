package grpc

import "github.com/go-kratos/kratos/v2/log"

const (
	clientSharedPluginResourceName     = clientPluginName + ".plugin"
	clientSharedReadinessResourceName  = clientPluginName + ".readiness"
	clientSharedHealthResourceName     = clientPluginName + ".health"
	clientPrivateReadinessResourceName = "readiness"
	clientPrivateHealthResourceName    = "health"

	serviceSharedPluginResourceName     = pluginName + ".plugin"
	serviceSharedReadinessResourceName  = pluginName + ".readiness"
	serviceSharedHealthResourceName     = pluginName + ".health"
	servicePrivateReadinessResourceName = "readiness"
	servicePrivateHealthResourceName    = "health"
)

func (c *ClientPlugin) registerRuntimePluginAlias() {
	if c == nil || c.rt == nil {
		return
	}
	if err := c.rt.RegisterSharedResource(clientSharedPluginResourceName, c); err != nil {
		log.Warnf("failed to register gRPC client shared plugin alias: %v", err)
	}
}

func (c *ClientPlugin) publishRuntimeContract(ready, healthy bool) {
	if c == nil || c.rt == nil {
		return
	}
	for _, item := range []struct {
		name  string
		value any
	}{
		{name: clientSharedReadinessResourceName, value: ready},
		{name: clientSharedHealthResourceName, value: healthy},
	} {
		if err := c.rt.RegisterSharedResource(item.name, item.value); err != nil {
			log.Warnf("failed to register gRPC client shared runtime contract %s: %v", item.name, err)
		}
	}
	if err := c.rt.RegisterPrivateResource(clientPrivateReadinessResourceName, ready); err != nil {
		log.Warnf("failed to register gRPC client private readiness resource: %v", err)
	}
	if err := c.rt.RegisterPrivateResource(clientPrivateHealthResourceName, healthy); err != nil {
		log.Warnf("failed to register gRPC client private health resource: %v", err)
	}
}

func (g *Service) registerRuntimePluginAlias() {
	if g == nil || g.rt == nil {
		return
	}
	if err := g.rt.RegisterSharedResource(serviceSharedPluginResourceName, g); err != nil {
		log.Warnf("failed to register gRPC service shared plugin alias: %v", err)
	}
}

func (g *Service) publishRuntimeContract(ready, healthy bool) {
	if g == nil || g.rt == nil {
		return
	}
	for _, item := range []struct {
		name  string
		value any
	}{
		{name: serviceSharedReadinessResourceName, value: ready},
		{name: serviceSharedHealthResourceName, value: healthy},
	} {
		if err := g.rt.RegisterSharedResource(item.name, item.value); err != nil {
			log.Warnf("failed to register gRPC service shared runtime contract %s: %v", item.name, err)
		}
	}
	if err := g.rt.RegisterPrivateResource(servicePrivateReadinessResourceName, ready); err != nil {
		log.Warnf("failed to register gRPC service private readiness resource: %v", err)
	}
	if err := g.rt.RegisterPrivateResource(servicePrivateHealthResourceName, healthy); err != nil {
		log.Warnf("failed to register gRPC service private health resource: %v", err)
	}
}
