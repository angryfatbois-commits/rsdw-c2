package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"

	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

// Helm invokes the same executable before applying any restore-target resources.
func init() {
	if len(os.Args) != 2 || os.Args[1] != "--render-stopped-workloads" {
		return
	}
	if err := renderStoppedWorkloads(os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func renderStoppedWorkloads(input io.Reader, output io.Writer) error {
	decoder := utilyaml.NewYAMLOrJSONDecoder(input, 4096)
	var rendered bytes.Buffer
	workloads := 0
	for {
		var resource map[string]any
		if err := decoder.Decode(&resource); err == io.EOF {
			break
		} else if err != nil {
			return err
		}
		if len(resource) == 0 {
			continue
		}
		metadata, _ := resource["metadata"].(map[string]any)
		annotations, _ := metadata["annotations"].(map[string]any)
		if annotations["helm.sh/hook"] != nil {
			return fmt.Errorf("restore target cannot contain Helm hooks")
		}
		switch resource["kind"] {
		case "Deployment", "StatefulSet":
			spec, ok := resource["spec"].(map[string]any)
			if !ok {
				return fmt.Errorf("workload has no spec")
			}
			spec["replicas"] = 0
			workloads++
		case "ConfigMap", "Secret", "Service", "PersistentVolumeClaim", "ServiceAccount", "Role", "RoleBinding", "ClusterRole", "ClusterRoleBinding", "NetworkPolicy", "Ingress", "PodDisruptionBudget":
		default:
			return fmt.Errorf("unsupported resource in stopped restore target: %v", resource["kind"])
		}
		data, err := json.Marshal(resource)
		if err != nil {
			return err
		}
		rendered.WriteString("---\n")
		rendered.Write(data)
		rendered.WriteByte('\n')
	}
	if workloads == 0 {
		return fmt.Errorf("restore target contains no supported workload")
	}
	_, err := io.Copy(output, &rendered)
	return err
}
