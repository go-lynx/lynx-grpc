package grpc

import (
	"context"
	"testing"
	"time"

	"github.com/go-lynx/lynx-grpc/conf"
	"github.com/go-lynx/lynx/plugins"
	"google.golang.org/protobuf/types/known/durationpb"
)

func TestGrpcClientRuntimeContract_NoControlPlaneLifecycle(t *testing.T) {
	base := plugins.NewSimpleRuntime()
	rt := base.WithPluginContext(clientPluginName)

	plugin := NewGrpcClientPlugin()
	plugin.rt = rt
	plugin.conf = &conf.GrpcClient{}

	if err := plugin.StartContext(context.Background(), plugin); err != nil {
		t.Fatalf("StartContext failed: %v", err)
	}

	alias, err := base.GetSharedResource(clientSharedPluginResourceName)
	if err != nil {
		t.Fatalf("shared plugin alias: %v", err)
	}
	if alias != plugin {
		t.Fatalf("unexpected shared plugin alias: got %T want %T", alias, plugin)
	}

	if readiness, err := base.GetSharedResource(clientSharedReadinessResourceName); err != nil || readiness != true {
		t.Fatalf("unexpected client shared readiness: value=%#v err=%v", readiness, err)
	}
	if health, err := base.GetSharedResource(clientSharedHealthResourceName); err != nil || health != true {
		t.Fatalf("unexpected client shared health: value=%#v err=%v", health, err)
	}
	if required, err := base.GetSharedResource(requiredReadinessStableResourceName); err != nil || required != true {
		t.Fatalf("unexpected required readiness resource: value=%#v err=%v", required, err)
	}
	if _, err := rt.GetPrivateResource("config"); err != nil {
		t.Fatalf("private config resource missing: %v", err)
	}

	if err := plugin.CleanupTasks(); err != nil {
		t.Fatalf("CleanupTasks failed: %v", err)
	}

	if readiness, err := base.GetSharedResource(clientSharedReadinessResourceName); err != nil || readiness != false {
		t.Fatalf("unexpected client shared readiness after cleanup: value=%#v err=%v", readiness, err)
	}
	if health, err := base.GetSharedResource(clientSharedHealthResourceName); err != nil || health != false {
		t.Fatalf("unexpected client shared health after cleanup: value=%#v err=%v", health, err)
	}
}

func TestGrpcServiceRuntimeContract_LocalBootstrapLifecycle(t *testing.T) {
	base := plugins.NewSimpleRuntime()
	rt := base.WithPluginContext(pluginName)

	plugin := NewGrpcService()
	plugin.rt = rt
	plugin.conf = &conf.Service{
		Network: "tcp",
		Addr:    "127.0.0.1:0",
		Timeout: durationpb.New(time.Second),
	}
	plugin.serverOpts = buildServerOptionsFromConfig(plugin.conf, defaultServerOptions())

	if err := plugin.StartupTasks(); err != nil {
		t.Fatalf("StartupTasks failed: %v", err)
	}

	alias, err := base.GetSharedResource(serviceSharedPluginResourceName)
	if err != nil {
		t.Fatalf("shared plugin alias: %v", err)
	}
	if alias != plugin {
		t.Fatalf("unexpected shared plugin alias: got %T want %T", alias, plugin)
	}

	if readiness, err := base.GetSharedResource(serviceSharedReadinessResourceName); err != nil || readiness != true {
		t.Fatalf("unexpected service shared readiness: value=%#v err=%v", readiness, err)
	}
	if health, err := base.GetSharedResource(serviceSharedHealthResourceName); err != nil || health != true {
		t.Fatalf("unexpected service shared health: value=%#v err=%v", health, err)
	}
	if _, err := rt.GetPrivateResource("config"); err != nil {
		t.Fatalf("private config resource missing: %v", err)
	}
	if _, err := rt.GetPrivateResource("server"); err != nil {
		t.Fatalf("private server resource missing: %v", err)
	}

	if err := plugin.CleanupTasksContext(context.Background()); err != nil {
		t.Fatalf("CleanupTasksContext failed: %v", err)
	}

	if readiness, err := base.GetSharedResource(serviceSharedReadinessResourceName); err != nil || readiness != false {
		t.Fatalf("unexpected service shared readiness after cleanup: value=%#v err=%v", readiness, err)
	}
	if health, err := base.GetSharedResource(serviceSharedHealthResourceName); err != nil || health != false {
		t.Fatalf("unexpected service shared health after cleanup: value=%#v err=%v", health, err)
	}
}
