// Package pythonruntime embeds the shared Python model policy and worker sources in the binary.
// It contains no machine configuration or workload state.
package pythonruntime

import _ "embed"

//go:generate python3 ../../../../../ml/scripts/bundle_native.py
//go:embed runtime.zip
var archive string

// Bytes returns an independent copy so a driver cannot modify another driver's source bundle.
func Bytes() []byte { return []byte(archive) }
