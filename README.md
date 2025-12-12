# PVC Shrink Ray 🔫

A Kubernetes MutatingAdmissionWebhook that zaps inflated PVCs back to their correct `RestoreSize`.

It fixes VM snapshot restore failures on strict CSI drivers (NetApp Trident, etc.) caused by CDI's filesystem overhead inflation.

## Problem

When restoring a VM from a VolumeSnapshot, the restore fails on storage backends that enforce `requestedSize <= RestoreSize`:

```
error creating PVC for snapshot restore: requested PVC size '22229813' is too large for the clone source '20971520'
```

This happens specifically for **persistent-state PVCs** (TPM/EFI firmware state) during VM snapshot restores, while regular VM disk restores work fine.

### Root Cause: The `cdi.kubevirt.io/applyStorageProfile` Label

KubeVirt's backend storage controller adds a special label to persistent-state PVCs:

```go
pvc.Labels["cdi.kubevirt.io/applyStorageProfile"] = "true"
```

This label triggers CDI's `pvcMutatingWebhook` to apply StorageProfile settings, including filesystem overhead inflation via `InflateSizeWithOverhead()`:

```go
// Adds ~5.5% overhead for ext4 filesystem metadata
func InflateSizeWithOverhead(ctx context.Context, client client.Client, size int64, storageClass *string) int64 {
    // Default overhead is 0.055 (5.5%)
    fsOverhead := GetFilesystemOverhead(ctx, client, storageClass)
    return int64(math.Ceil(float64(size) / (1 - fsOverhead)))
}
```

**The bug:** CDI inflates PVC size even when restoring from a VolumeSnapshot, where `RestoreSize` is a hard constraint that cannot be exceeded.

### Why Only Persistent-State PVCs Fail

| PVC Type | Has `applyStorageProfile` Label | CDI Webhook Inflates | Result |
|----------|--------------------------------|---------------------|--------|
| Regular VM disk (forklift-migrated) | ❌ No | No | ✅ Works |
| DataVolume-created PVC | ❌ No | No | ✅ Works |
| Persistent-state (TPM/EFI) | ✅ Yes | **Yes (+6%)** | ❌ **Fails** |

### The Complete Bug Flow

```
1. User creates VM snapshot
   └─ VolumeSnapshot created with RestoreSize: 20Mi

2. User restores VM from snapshot
   └─ KubeVirt restore controller calls CreateRestorePVCDef()

3. CreateRestorePVCDef() copies labels from source PVC
   └─ Labels include: cdi.kubevirt.io/applyStorageProfile=true

4. PVC created with RestoreSize (20Mi) and the label
   └─ Kubernetes API receives PVC creation request

5. CDI's pvcMutatingWebhook intercepts (has the trigger label!)
   └─ Calls RenderPvc() → renderPvcSpecVolumeSize() → InflateSizeWithOverhead()
   └─ 20Mi → 22229813 bytes (~21.2Mi, +6% overhead)

6. CSI driver (NetApp Trident) rejects the restore
   └─ 22229813 > 20971520 (RestoreSize)
   └─ Error: "requested PVC size is too large for the clone source"
```

## Solution

This webhook runs **after** CDI's `pvcMutatingWebhook` and shrinks the PVC size back to the VolumeSnapshot's `RestoreSize`.

**Example transformation:**

Before (after CDI inflation):
```yaml
spec:
  dataSource:
    kind: VolumeSnapshot
    name: my-vm-snapshot
  resources:
    requests:
      storage: 22229813  # Inflated by CDI (+6%)
```

After (PVC Shrink Ray fix):
```yaml
spec:
  dataSource:
    kind: VolumeSnapshot
    name: my-vm-snapshot
  resources:
    requests:
      storage: 20Mi  # Shrunk back to RestoreSize
```

### How It Works

```
PVC Create → CDI Webhook (inflates) → PVC Shrink Ray (shrinks) → CSI Driver
   20Mi    →      22229813          →         20Mi             →    ✅ Success
```

The webhook:
1. Intercepts PVCs with `cdi.kubevirt.io/applyStorageProfile=true` label
2. Checks if DataSource is a VolumeSnapshot (supports both `dataSource` and `dataSourceRef`)
3. Fetches the VolumeSnapshot's `status.restoreSize`
4. If `spec.resources.requests.storage > restoreSize`, patches it back to `restoreSize`

**When to use this webhook:**

Use when all of the following are true:
- OpenShift Virtualization (CNV) / KubeVirt with VM snapshots
- CSI driver enforces `requestedSize <= RestoreSize` (NetApp Trident, etc.)
- VMs have persistent state (TPM or EFI with secure boot)

Skip if your CSI driver allows over-provisioning on snapshot restores.

## Deployment

### Prerequisites

- OpenShift 4.x cluster with OpenShift Virtualization (CNV)
- `oc` CLI configured and authenticated

### Deploy

The deployment uses a prebuilt image published to `ghcr.io/grandeit/pvc-shrink-ray:latest` via GitHub Actions on every push to `main`.

```bash
oc apply -k deploy/
```

Verify deployment:

```bash
oc get pods -n openshift-cnv -l app=pvc-shrink-ray
oc logs -n openshift-cnv -l app=pvc-shrink-ray -f
```

### High Availability

The deployment includes:
- **2 replicas** for redundancy
- **Pod anti-affinity** ensuring pods run on different nodes
- **PodDisruptionBudget** maintaining at least 1 pod during voluntary disruptions
- **Automatic TLS certificate management** via OpenShift's service-ca-operator

## Troubleshooting

Check the webhook logs for "Zapping" messages when restoring VMs from snapshots:

```bash
oc logs -n openshift-cnv -l app=pvc-shrink-ray -f
```

You should see:
```
Zapping oversized PVC openshift-cnv/restore-xyz! Reducing size from 22229813 to RestoreSize 20Mi (VolumeSnapshot: openshift-cnv/my-vm-snapshot)
```

Verify the PVC was created with the correct size:

```bash
oc get pvc <restore-pvc> -o jsonpath='{.spec.resources.requests.storage}'
```

## Uninstall

```bash
oc delete -k deploy/
```