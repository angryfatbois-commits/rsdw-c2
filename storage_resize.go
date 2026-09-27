package main

import (
	"context"
	"encoding/json"
	"fmt"
)

// resizeWorldClaim grows a server's world PersistentVolumeClaim to storageGiB.
// Kubernetes' API server rejects a storage-request increase at admission time
// when the claim's StorageClass does not set allowVolumeExpansion, so the
// caller can propagate the returned error directly to the operator without a
// separate, potentially stale capability check.
func (k *kubeOrchestrator) resizeWorldClaim(ctx context.Context, server Server, storageGiB int) error {
	claim, err := k.worldClaim(ctx, server)
	if err != nil {
		return err
	}
	patch, err := json.Marshal(map[string]any{
		"spec": map[string]any{
			"resources": map[string]any{
				"requests": map[string]any{
					"storage": fmt.Sprintf("%dGi", storageGiB),
				},
			},
		},
	})
	if err != nil {
		return err
	}
	if _, err := k.runner.Run(ctx, k.kubectl, "-n", server.Namespace, "patch", "persistentvolumeclaims/"+claim, "--type=merge", "-p", string(patch)); err != nil {
		return fmt.Errorf("could not grow the world volume: %w", err)
	}
	return nil
}
