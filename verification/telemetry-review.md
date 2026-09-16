# Telemetry review record

## Review scope

Separate inherited-model agents reviewed the design, UI, and backend. This was not a multi-model review.

The UI review found four issues: chart bounds did not exclude old source timestamps, series tables reused the wrong timestamp, outbound-only traffic lacked its latest-value marker, and refresh closed expanded data tables. The implementation now filters timestamps before scaling, lists each series separately, draws both series markers, and restores open disclosures. `web/app.test.cjs` checks chart bounds, source timestamps, missing data, and outbound markers. Disclosure persistence requires a browser check.

The backend review found shared timeout suppression, update checks without container identity revalidation, misleading legacy numeric fields, and a no-collection-on-read test that only exercised unauthorized requests. Regression fixes and their final verification are recorded in the telemetry verification report.

Source calls now have individual 1.5-second deadlines inside a 12-second sweep budget, with time reserved for final container identity verification. Update checks only discover the image and no longer collect game measurements. They reject an absent observed image before a registry call. Live JSON omits legacy numeric metric fields. The read-only assertion now sends authenticated requests and requires HTTP 200.

The comment review removed the capacity and legacy-field narration. It retained the comment about kubectl exec addressing names rather than UIDs, an external behavior that motivates container revalidation. No constraints were left awaiting approval.

## Evidence corrections

The initial shutdown audit row cited a kubeconfig, which identifies the cluster but does not prove that it stopped. The later shutdown row cited a historical setup document, which does not prove that the demo listener stopped. Direct verification on September 16, 2026 returned `exited` from Docker for `rsdw-c2-live-control-plane`, with its retained 3 GiB memory cap and 2 CPU quota. `ss -ltnp` found no listeners on 18081 or 18083. Neither the node nor world storage was deleted.

The initial UI audit entries describe unit-level verification, not browser verification. The final verification report distinguishes those checks.

## First Kubernetes run

Metrics Server v0.9.0 installed successfully on disposable kind Kubernetes v1.36.1. Its APIService became available. C2's pod metric permissions passed, and node metric, node stats, and node proxy access were denied as intended.

The game fixture failed readiness because the production Alpine image does not include `httpd`. This was a fixture defect, not successful telemetry verification. A separate test-only Dockerfile now installs `busybox-extras`; the production image remains unchanged. The failed disposable cluster was deleted. Logs remain at `/tmp/rsdw-c2-kind.rlsRHR` on the test laptop.
