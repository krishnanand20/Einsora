package manifests

import (
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/krishnanand20/tensorafi/controlplane/internal/cluster"
	"github.com/krishnanand20/tensorafi/controlplane/internal/domain"
)

// virtualMachine is a KubeVirt VM with whole GPUs passed through via VFIO. The root disk is
// cloned from a DataSource in the images namespace and survives stop/start.
func virtualMachine(w domain.Workload, c Context, running bool) cluster.Manifest {
	name := cluster.ObjectName(w.ID)
	lbl := labels(w, c)
	runStrategy := "Halted"
	if running {
		runStrategy = "Always"
	}
	gpus := make([]any, w.GPUCount)
	for i := range gpus {
		gpus[i] = map[string]any{"name": fmt.Sprintf("gpu%d", i), "deviceName": c.GPU.VMDeviceName}
	}
	return cluster.Manifest{
		"apiVersion": "kubevirt.io/v1",
		"kind":       "VirtualMachine",
		"metadata":   metadata(name, cluster.TenantNamespace(c.TenantSlug), lbl, annotations(w, running)),
		"spec": map[string]any{
			"runStrategy": runStrategy,
			"dataVolumeTemplates": []any{map[string]any{
				"metadata": map[string]any{"name": name + "-root", "labels": lbl},
				"spec": map[string]any{
					"sourceRef": map[string]any{"kind": "DataSource", "name": c.Runtime.Ref, "namespace": c.ImagesNamespace},
					"storage": map[string]any{
						"storageClassName": c.StorageClass,
						"resources":        map[string]any{"requests": map[string]any{"storage": fmt.Sprintf("%dGi", c.Image.RootDiskGiB)}},
					},
				},
			}},
			"template": map[string]any{
				"metadata": map[string]any{"labels": lbl},
				"spec": map[string]any{
					"domain": map[string]any{
						"cpu":    map[string]any{"cores": int64(c.GPU.VCPUsPerGPU * w.GPUCount)},
						"memory": map[string]any{"guest": fmt.Sprintf("%dGi", c.GPU.MemoryGiBPerGPU*w.GPUCount)},
						"devices": map[string]any{
							"gpus": gpus,
							"disks": []any{
								map[string]any{"name": "root", "disk": map[string]any{"bus": "virtio"}},
								map[string]any{"name": "cloudinit", "disk": map[string]any{"bus": "virtio"}},
							},
						},
					},
					"volumes": []any{
						map[string]any{"name": "root", "dataVolume": map[string]any{"name": name + "-root"}},
						map[string]any{"name": "cloudinit", "cloudInitNoCloud": map[string]any{
							"userData": cloudConfig(w.Params[domain.ParamSSHPublicKey]),
						}},
					},
				},
			},
		},
	}
}

// cloudConfig renders user-data as JSON (valid YAML), so no input can break out of its field.
func cloudConfig(sshKey string) string {
	body, _ := json.Marshal(map[string]any{
		"ssh_authorized_keys": []string{sshKey},
		"ssh_pwauth":          false,
		"disable_root":        true,
	})
	return "#cloud-config\n" + string(body)
}

// notebook is JupyterLab in a StatefulSet. Scaling to zero parks it; the PVC is retained.
func notebook(w domain.Workload, c Context, running bool) []cluster.Manifest {
	name := cluster.ObjectName(w.ID)
	ns := cluster.TenantNamespace(c.TenantSlug)
	lbl := labels(w, c)
	replicas := int64(0)
	if running {
		replicas = 1
	}
	pod := hardenPod(c, map[string]any{
		"securityContext": map[string]any{
			"runAsNonRoot": true, "runAsUser": int64(1000), "fsGroup": int64(1000),
			"seccompProfile": map[string]any{"type": "RuntimeDefault"},
		},
		"containers": []any{map[string]any{
			"name":            "notebook",
			"image":           c.Runtime.Ref,
			"ports":           []any{map[string]any{"name": "http", "containerPort": int64(8888)}},
			"resources":       gpuResources(c, w.GPUCount),
			"securityContext": containerSecurityContext(),
			"volumeMounts":    []any{map[string]any{"name": "work", "mountPath": "/home/jovyan/work"}},
		}},
	})
	return []cluster.Manifest{
		{
			"apiVersion": "v1",
			"kind":       "Service",
			"metadata":   metadata(name, ns, lbl, nil),
			"spec": map[string]any{
				"selector": map[string]any{cluster.LabelWorkloadID: w.ID.String()},
				"ports":    []any{map[string]any{"name": "http", "port": int64(80), "targetPort": "http"}},
			},
		},
		{
			"apiVersion": "apps/v1",
			"kind":       "StatefulSet",
			"metadata":   metadata(name, ns, lbl, annotations(w, running)),
			"spec": map[string]any{
				"replicas":    replicas,
				"serviceName": name,
				"selector":    map[string]any{"matchLabels": map[string]any{cluster.LabelWorkloadID: w.ID.String()}},
				"template":    map[string]any{"metadata": map[string]any{"labels": lbl}, "spec": pod},
				"volumeClaimTemplates": []any{map[string]any{
					"metadata": map[string]any{"name": "work", "labels": lbl},
					"spec": map[string]any{
						"accessModes":      anyList("ReadWriteOnce"),
						"storageClassName": c.StorageClass,
						"resources":        map[string]any{"requests": map[string]any{"storage": "50Gi"}},
					},
				}},
			},
		},
	}
}

// inferenceService serves a model with the tensorafi-vllm ClusterServingRuntime, which pins
// --load-format=safetensors and never enables trust_remote_code (AGENTS.md rule 6).
func inferenceService(w domain.Workload, c Context) cluster.Manifest {
	lbl := labels(w, c)
	ann := annotations(w, true)
	ann["serving.kserve.io/deploymentMode"] = "RawDeployment"
	model := map[string]any{
		"modelFormat": map[string]any{"name": "huggingface"},
		"runtime":     c.Runtime.Ref,
		"storageUri":  w.Params[domain.ParamModelURI],
		"resources":   gpuResources(c, w.GPUCount),
	}
	if w.GPUCount > 1 {
		model["args"] = anyList("--tensor-parallel-size=" + strconv.Itoa(w.GPUCount))
	}
	predictor := hardenPod(c, map[string]any{
		"minReplicas": int64(1),
		"maxReplicas": int64(1),
		"model":       model,
	})
	return cluster.Manifest{
		"apiVersion": "serving.kserve.io/v1beta1",
		"kind":       "InferenceService",
		"metadata":   metadata(cluster.ObjectName(w.ID), cluster.TenantNamespace(c.TenantSlug), lbl, ann),
		"spec":       map[string]any{"predictor": predictor},
	}
}
