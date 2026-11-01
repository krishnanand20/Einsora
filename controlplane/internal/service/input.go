package service

import (
	"net/url"
	"regexp"
	"strings"
	"unicode"

	"github.com/krishnanand20/tensorafi/controlplane/internal/domain"
)

const maxGPUsPerWorkload = 8

// CreateWorkloadInput is the POST /v1/workloads body.
type CreateWorkloadInput struct {
	Name         string              `json:"name"`
	Kind         domain.WorkloadKind `json:"kind"`
	ImageID      string              `json:"image_id"`
	GPUClass     string              `json:"gpu_class"`
	GPUCount     int                 `json:"gpu_count"`
	SSHPublicKey string              `json:"ssh_public_key,omitempty"` // vm only
	ModelURI     string              `json:"model_uri,omitempty"`      // endpoint only
	// Region is where it runs and its data stays (ADR-0009); empty means the catalog's default.
	Region domain.Region `json:"region,omitempty"`
}

func invalid(format string, args ...any) error {
	return domain.Errorf(domain.CodeInvalid, format, args...)
}

func (in CreateWorkloadInput) validate() error {
	switch {
	case !domain.ValidSlug(in.Name):
		return invalid("name must be 1-40 lowercase letters, digits, or '-', starting and ending with a letter or digit")
	case !in.Kind.Valid():
		return invalid("kind must be vm, notebook, or endpoint")
	case in.GPUCount < 1 || in.GPUCount > maxGPUsPerWorkload:
		return invalid("gpu_count must be between 1 and %d", maxGPUsPerWorkload)
	}
	switch in.Kind {
	case domain.KindVM:
		if in.ModelURI != "" {
			return invalid("model_uri is only valid for endpoints")
		}
		if !validSSHKey(in.SSHPublicKey) {
			return invalid("ssh_public_key must be a single OpenSSH public key line (ssh-ed25519, ssh-rsa, or ecdsa)")
		}
	case domain.KindEndpoint:
		if in.SSHPublicKey != "" {
			return invalid("ssh_public_key is only valid for VMs")
		}
		if !validModelURI(in.ModelURI) {
			return invalid("model_uri must be an s3:// or hf:// URI with no credentials in it")
		}
	case domain.KindNotebook:
		if in.SSHPublicKey != "" || in.ModelURI != "" {
			return invalid("notebooks take neither ssh_public_key nor model_uri")
		}
	}
	return nil
}

func (in CreateWorkloadInput) params() map[string]string {
	p := map[string]string{}
	if in.SSHPublicKey != "" {
		p[domain.ParamSSHPublicKey] = in.SSHPublicKey
	}
	if in.ModelURI != "" {
		p[domain.ParamModelURI] = in.ModelURI
	}
	return p
}

var sshKeyTypes = []string{
	"ssh-ed25519 ", "sk-ssh-ed25519@openssh.com ", "ssh-rsa ",
	"ecdsa-sha2-nistp256 ", "ecdsa-sha2-nistp384 ", "ecdsa-sha2-nistp521 ",
}

// validSSHKey accepts exactly one key line. Rejecting control characters keeps the key
// from smuggling extra directives into cloud-init.
func validSSHKey(k string) bool {
	if len(k) > 4096 || strings.IndexFunc(k, unicode.IsControl) >= 0 {
		return false
	}
	for _, prefix := range sshKeyTypes {
		if strings.HasPrefix(k, prefix) && len(k) > len(prefix)+16 {
			return true
		}
	}
	return false
}

// modelHost is an S3 bucket or Hugging Face organization: no port, no credentials.
var modelHost = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// validModelURI accepts a well-formed s3:// or hf:// URI. Anything that could carry credentials
// (an @ anywhere, or a port, which is how url.Parse reads "user:1234/rest-of-password@") is
// refused: credentials would be copied into the runtime and every log that echoes the URI.
func validModelURI(u string) bool {
	if len(u) > 1024 || strings.ContainsRune(u, '@') ||
		strings.IndexFunc(u, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return false
	}
	parsed, err := url.Parse(u)
	if err != nil || parsed.User != nil || !modelHost.MatchString(parsed.Host) {
		return false
	}
	for _, scheme := range []string{"s3://", "hf://"} {
		if rest, ok := strings.CutPrefix(u, scheme); ok && rest != "" && !strings.Contains(rest, "..") {
			return true
		}
	}
	return false
}
