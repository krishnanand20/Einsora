package manifests

import (
	"maps"
	"strconv"

	"github.com/krishnanand20/einsora/controlplane/internal/cluster"
)

// privateRanges are blocked for tenant egress: node IPs, other pods, the metadata range,
// and anything else on our side of the network. Tenants get the public internet only.
var privateRanges = []string{
	"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10", "169.254.0.0/16",
}

// Tenant returns the per-tenant bundle: namespace, GPU ResourceQuota (a backstop — the
// authoritative quota check is in Postgres), network isolation, and permission to clone
// VM images. gpuQuota maps extended resource name → GPUs.
func Tenant(slug string, gpuQuota map[string]int, imagesNamespace string) []cluster.Manifest {
	ns := cluster.TenantNamespace(slug)
	lbl := map[string]any{cluster.LabelManagedBy: cluster.ManagedBy, cluster.LabelTenant: slug}

	nsLabels := maps.Clone(lbl)
	// TODO(phase-3): confirm virt-launcher runs under "baseline" with the installed KubeVirt version.
	nsLabels["pod-security.kubernetes.io/enforce"] = "baseline"
	nsLabels["pod-security.kubernetes.io/warn"] = "restricted"

	hard := map[string]any{}
	for resource, n := range gpuQuota {
		hard["requests."+resource] = strconv.Itoa(n)
	}

	return []cluster.Manifest{
		{"apiVersion": "v1", "kind": "Namespace", "metadata": metadata(ns, "", nsLabels, nil)},
		{
			"apiVersion": "v1",
			"kind":       "ResourceQuota",
			"metadata":   metadata("gpu-quota", ns, lbl, nil),
			"spec":       map[string]any{"hard": hard},
		},
		tenantNetworkPolicy(ns, lbl),
		gatewayIngressPolicy(ns, lbl),
		{
			"apiVersion": "rbac.authorization.k8s.io/v1",
			"kind":       "RoleBinding",
			"metadata":   metadata("clone-images-"+slug, imagesNamespace, lbl, nil),
			"roleRef": map[string]any{
				"apiGroup": "rbac.authorization.k8s.io", "kind": "ClusterRole", "name": "einsora-image-cloner",
			},
			"subjects": []any{map[string]any{
				"kind": "Group", "apiGroup": "rbac.authorization.k8s.io", "name": "system:serviceaccounts:" + ns,
			}},
		},
	}
}

func tenantNetworkPolicy(ns string, lbl map[string]any) cluster.Manifest {
	allPodsHere := map[string]any{"podSelector": map[string]any{}}
	return cluster.Manifest{
		"apiVersion": "networking.k8s.io/v1",
		"kind":       "NetworkPolicy",
		"metadata":   metadata("tenant-baseline", ns, lbl, nil),
		"spec": map[string]any{
			"podSelector": map[string]any{},
			"policyTypes": anyList("Ingress", "Egress"),
			"ingress":     []any{map[string]any{"from": []any{allPodsHere}}},
			"egress": []any{
				map[string]any{"to": []any{allPodsHere}},
				map[string]any{
					"to": []any{map[string]any{
						"namespaceSelector": map[string]any{"matchLabels": map[string]any{"kubernetes.io/metadata.name": "kube-system"}},
						"podSelector":       map[string]any{"matchLabels": map[string]any{"k8s-app": "kube-dns"}},
					}},
					"ports": []any{
						map[string]any{"protocol": "UDP", "port": int64(53)},
						map[string]any{"protocol": "TCP", "port": int64(53)},
					},
				},
				map[string]any{"to": []any{map[string]any{
					"namespaceSelector": map[string]any{"matchLabels": map[string]any{cluster.LabelRole: cluster.RoleModelStore}},
				}}},
				map[string]any{"to": []any{map[string]any{
					"ipBlock": map[string]any{"cidr": "0.0.0.0/0", "except": anyList(privateRanges...)},
				}}},
			},
		},
	}
}

// gatewayIngressPolicy admits traffic from Cilium's Gateway API proxy, which reaches pods
// with the reserved "ingress" identity that plain NetworkPolicy selectors cannot match.
func gatewayIngressPolicy(ns string, lbl map[string]any) cluster.Manifest {
	return cluster.Manifest{
		"apiVersion": "cilium.io/v2",
		"kind":       "CiliumNetworkPolicy",
		"metadata":   metadata("allow-from-gateway", ns, lbl, nil),
		"spec": map[string]any{
			"endpointSelector": map[string]any{},
			"ingress":          []any{map[string]any{"fromEntities": anyList("ingress")}},
		},
	}
}
