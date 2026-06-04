package filter

import (
	"context"
	"strconv"
	"strings"

	"github.com/go-kratos/kratos/v2/registry"
	"github.com/go-kratos/kratos/v2/selector"
)

// Version keeps nodes whose "version" metadata matches version. Nodes lacking the
// metadata are kept (fail-open), and if the filter would leave nothing it returns
// all nodes so the service stays reachable.
func Version(version string) selector.NodeFilter {
	return func(ctx context.Context, nodes []selector.Node) []selector.Node {
		if version == "" {
			return nodes
		}

		var filteredNodes []selector.Node
		for _, node := range nodes {
			if nodeVersion, exists := node.Metadata()["version"]; exists {
				if nodeVersion == version {
					filteredNodes = append(filteredNodes, node)
				}
			} else {
				filteredNodes = append(filteredNodes, node)
			}
		}

		if len(filteredNodes) == 0 {
			return nodes
		}

		return filteredNodes
	}
}

// Group keeps nodes whose "group" metadata matches group; fail-open like Version.
func Group(group string) selector.NodeFilter {
	return func(ctx context.Context, nodes []selector.Node) []selector.Node {
		if group == "" {
			return nodes
		}

		var filteredNodes []selector.Node
		for _, node := range nodes {
			if nodeGroup, exists := node.Metadata()["group"]; exists {
				if nodeGroup == group {
					filteredNodes = append(filteredNodes, node)
				}
			} else {
				filteredNodes = append(filteredNodes, node)
			}
		}

		if len(filteredNodes) == 0 {
			return nodes
		}

		return filteredNodes
	}
}

// Healthy drops nodes whose "health" metadata is not "healthy"/"up". Nodes without
// the metadata are assumed healthy; if all are filtered out, all are returned (fail-open).
func Healthy() selector.NodeFilter {
	return func(ctx context.Context, nodes []selector.Node) []selector.Node {
		var healthyNodes []selector.Node
		for _, node := range nodes {
			if healthStatus, exists := node.Metadata()["health"]; exists {
				if healthStatus == "healthy" || healthStatus == "up" {
					healthyNodes = append(healthyNodes, node)
				}
			} else {
				healthyNodes = append(healthyNodes, node)
			}
		}

		if len(healthyNodes) == 0 {
			return nodes
		}

		return healthyNodes
	}
}

// Region keeps nodes whose "region" metadata matches region; fail-open like Version.
func Region(region string) selector.NodeFilter {
	return func(ctx context.Context, nodes []selector.Node) []selector.Node {
		if region == "" {
			return nodes
		}

		var filteredNodes []selector.Node
		for _, node := range nodes {
			if nodeRegion, exists := node.Metadata()["region"]; exists {
				if nodeRegion == region {
					filteredNodes = append(filteredNodes, node)
				}
			} else {
				filteredNodes = append(filteredNodes, node)
			}
		}

		if len(filteredNodes) == 0 {
			return nodes
		}

		return filteredNodes
	}
}

// RegistryNode wraps a registry service instance
type RegistryNode struct {
	instance *registry.ServiceInstance
}

// NewRegistryNode creates a new registry node
func NewRegistryNode(instance *registry.ServiceInstance) *RegistryNode {
	return &RegistryNode{
		instance: instance,
	}
}

// Scheme returns the scheme of the node
func (n *RegistryNode) Scheme() string {
	if len(n.instance.Endpoints) > 0 {
		// Extract scheme from endpoint (e.g., "grpc://" from "grpc://localhost:8080")
		for _, endpoint := range n.instance.Endpoints {
			if len(endpoint) > 0 {
				if idx := strings.Index(endpoint, "://"); idx > 0 {
					return endpoint[:idx]
				}
			}
		}
	}
	return "grpc" // Default to grpc
}

// Address returns the address of the node
func (n *RegistryNode) Address() string {
	if len(n.instance.Endpoints) > 0 {
		return n.instance.Endpoints[0]
	}
	return ""
}

// ServiceName returns the service name of the node
func (n *RegistryNode) ServiceName() string {
	return n.instance.Name
}

// InitialWeight returns the initial weight of the node
func (n *RegistryNode) InitialWeight() *int64 {
	// Try to get weight from metadata
	if weightStr, exists := n.instance.Metadata["weight"]; exists {
		if weight, err := strconv.ParseInt(weightStr, 10, 64); err == nil && weight > 0 {
			return &weight
		}
	}
	// Default weight
	defaultWeight := int64(100)
	return &defaultWeight
}

// Metadata returns the metadata of the node
func (n *RegistryNode) Metadata() map[string]string {
	return n.instance.Metadata
}
