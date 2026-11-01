package cluster

import (
	"strings"

	"github.com/google/uuid"
)

// Labels and annotations on every object TensoraFi creates. Selectors in deploy/ and the
// observability stack depend on these names; change them only with a migration plan.
const (
	Prefix = "tensorafi.dev/"

	LabelTenant       = Prefix + "tenant"
	LabelWorkloadID   = Prefix + "workload-id"
	LabelWorkloadKind = Prefix + "workload-kind"
	LabelGPUClass     = Prefix + "gpu-class"
	// LabelRole marks platform namespaces tenant NetworkPolicies refer to.
	LabelRole      = Prefix + "role"
	RoleGateway    = "gateway"
	RoleModelStore = "model-store"

	AnnotationGPUCount       = Prefix + "gpu-count"
	AnnotationDesiredRunning = Prefix + "desired-running"

	LabelManagedBy = "app.kubernetes.io/managed-by"
	ManagedBy      = "tensorafi"

	// FieldManager is the server-side-apply field manager for everything the controller owns.
	FieldManager = "tensorafi-controller"
)

// TenantNamespace is where all of a tenant's objects live (AGENTS.md rule 3).
func TenantNamespace(slug string) string { return "t-" + slug }

// ObjectName is the name of a workload's primary object: "w-" + 32 hex chars.
// Tenant-chosen names are never used in cluster object names.
func ObjectName(id uuid.UUID) string { return "w-" + strings.ReplaceAll(id.String(), "-", "") }
