package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

func normalizeAdminIDs(value string) (string, error) {
	ids := make([]string, 0)
	seen := map[string]bool{}
	for _, part := range strings.Split(value, ",") {
		id := strings.TrimSpace(part)
		if id == "" {
			continue
		}
		normalized, err := normalizePlayerID(id)
		if err != nil {
			return "", fmt.Errorf("administrator ID %q is invalid: %w", id, err)
		}
		if seen[normalized] {
			continue
		}
		seen[normalized] = true
		ids = append(ids, normalized)
	}
	return strings.Join(ids, ","), nil
}

func newPasswordUpdate(serverPassword, adminPassword *string) *PasswordUpdate {
	if serverPassword == nil && adminPassword == nil {
		return nil
	}
	copyValue := func(value *string) *string {
		if value == nil {
			return nil
		}
		copied := *value
		return &copied
	}
	return &PasswordUpdate{
		ServerPassword: copyValue(serverPassword),
		AdminPassword:  copyValue(adminPassword),
		Revision:       randomID(),
	}
}

func (k *kubeOrchestrator) updateOwnedPasswordSecret(ctx context.Context, server Server) error {
	update := server.PasswordUpdate
	if update == nil {
		return nil
	}
	if server.PasswordSecret == "" {
		return errors.New("password Secret name is missing")
	}
	live, err := k.deletionGet(ctx, "Secret", server.Namespace, server.PasswordSecret)
	if err != nil {
		return err
	}
	if live.Metadata.UID == "" {
		values := map[string]string{"serverPassword": "", "adminPassword": ""}
		if update.ServerPassword != nil {
			values["serverPassword"] = *update.ServerPassword
		}
		if update.AdminPassword != nil {
			values["adminPassword"] = *update.AdminPassword
		}
		return k.runJSONFile(ctx, map[string]any{
			"apiVersion": "v1",
			"kind":       "Secret",
			"metadata": map[string]any{
				"name":      server.PasswordSecret,
				"namespace": server.Namespace,
				"annotations": map[string]string{
					ownershipAnnotation: server.OwnershipToken,
				},
			},
			"stringData": values,
		}, "create")
	}
	if live.Metadata.Annotations[ownershipAnnotation] != server.OwnershipToken {
		return errors.New("Secret already exists without this server's ownership marker")
	}
	if live.Metadata.ResourceVersion == "" {
		return errors.New("cannot prove the current password Secret version")
	}
	data := map[string]string{}
	if update.ServerPassword != nil {
		data["serverPassword"] = base64.StdEncoding.EncodeToString([]byte(*update.ServerPassword))
	} else if _, ok := live.Data["serverPassword"]; !ok {
		data["serverPassword"] = ""
	}
	if update.AdminPassword != nil {
		data["adminPassword"] = base64.StdEncoding.EncodeToString([]byte(*update.AdminPassword))
	} else if _, ok := live.Data["adminPassword"]; !ok {
		data["adminPassword"] = ""
	}
	if len(data) == 0 {
		return nil
	}
	return k.runJSONPatchFile(ctx, map[string]any{
		"metadata": map[string]string{"resourceVersion": live.Metadata.ResourceVersion},
		"data":     data,
	}, "-n", server.Namespace, "patch", "secret/"+server.PasswordSecret, "--type=merge")
}

func (k *kubeOrchestrator) runJSONPatchFile(ctx context.Context, value any, args ...string) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp("", "rsdw-secret-patch-*.json")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if _, err := k.runner.Run(ctx, k.kubectl, append(args, "--patch-file", name)...); err != nil {
		return errors.New("Kubernetes Secret patch failed; inspect access and resource ownership")
	}
	return nil
}

func (k *kubeOrchestrator) patchDeploymentAnnotation(ctx context.Context, server Server, revision string) error {
	patch, err := json.Marshal(map[string]any{
		"spec": map[string]any{
			"template": map[string]any{
				"metadata": map[string]any{
					"annotations": map[string]string{restartAnnotation: revision},
				},
			},
		},
	})
	if err != nil {
		return err
	}
	if _, err := k.runner.Run(ctx, k.kubectl, "-n", server.Namespace, "patch", "deployment/"+deploymentName(server.Release), "--type=merge", "-p", string(patch)); err != nil {
		return errors.New("could not update the deployment rollout annotation")
	}
	return nil
}
