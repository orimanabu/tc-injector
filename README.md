# tc-injector

tc-injector is a Kubernetes operator that injects network latency into pods using the Linux `tc` (traffic control) command. It is designed for chaos engineering, performance testing, and fault injection scenarios where you need to simulate network delays between services.

## How It Works

tc-injector runs as a **DaemonSet** — one pod per node — and watches for `TCInjector` custom resources. Each `TCInjector` resource defines a list of rules, each pairing a pod label selector and a namespace selector with a delay range.

When a matching pod becomes ready on the node, tc-injector:

1. Connects to the node's CRI socket (containerd or CRI-O) to look up the container's PID.
2. Uses `nsenter --net=/proc/<pid>/ns/net` to enter the pod's network namespace.
3. Applies a `tc netem` qdisc directly to `eth0` (and any Multus-managed interfaces) **inside** the pod, injecting the specified delay on all **outgoing** traffic from the pod.

When the pod is deleted or no longer matches any rule, the qdisc is removed and normal scheduling is restored.

```
┌─────────────────────────────────────────────────────────┐
│  Kubernetes Node                                         │
│                                                          │
│  ┌──────────────┐     watches      ┌──────────────────┐ │
│  │  tc-injector │ ←────────────── │  TCInjector CRD  │ │
│  │  (DaemonSet) │                  └──────────────────┘ │
│  └──────┬───────┘                                        │
│         │  nsenter --net=/proc/<pid>/ns/net              │
│         ▼                                                 │
│  ┌─────────────────────────────────────────────────────┐ │
│  │  pod network namespace                               │ │
│  │                                                      │ │
│  │  eth0 ──[netem delay 30ms]──► outbound packets      │ │
│  │  net1 ──[netem delay 30ms]──► (Multus, if requested)│ │
│  └─────────────────────────────────────────────────────┘ │
└─────────────────────────────────────────────────────────┘
```

> **Direction**: tc delay is applied to **egress** traffic leaving the pod (outgoing packets).
> This is the opposite of applying tc to the host-side veth peer, which would delay ingress.
> For round-trip latency measurement (e.g. `ping`) the direction does not affect the observed RTT.

### Multus Interface Support

When a rule specifies `multusNetworks`, tc-injector also injects delay into interfaces added by [Multus CNI](https://github.com/k8snetworkplumbingwg/multus-cni) (e.g. ipvlan secondary interfaces). Because all interfaces — both the primary `eth0` and Multus-managed ones — are targeted **inside** the pod's network namespace via `nsenter`, the same code path handles both uniformly.

```
# All interfaces targeted inside the pod network namespace via nsenter
nsenter --net=/proc/<pid>/ns/net -- tc qdisc replace dev eth0 root handle 1: netem delay 30ms
nsenter --net=/proc/<pid>/ns/net -- tc qdisc replace dev net1 root handle 1: netem delay 30ms
```

Multus writes the list of attached interfaces and their NetworkAttachmentDefinition (NAD) names into the `k8s.v1.cni.cncf.io/network-status` pod annotation. tc-injector reads this annotation to resolve which interface name inside the pod corresponds to each requested NAD.

### Rule Matching

- Rules are evaluated in order; **the last matching rule wins**.
- The **pod selector** matches pods by their labels.
- The **namespace selector** matches pods by the labels on their namespace. An empty namespace selector matches all namespaces.
- Both selectors support `matchLabels` and `matchExpressions`.
- Only pods in the `Running` phase with all containers ready are targeted.

### Periodic Delay Rotation (optional)

When `enablePeriodicDelayRotation: true` is set, the controller periodically re-randomizes the delay for each injected pod within the configured `[minDelay, maxDelay]` range. This is useful for simulating realistic jitter.

## Prerequisites

- Kubernetes 1.26+ or OpenShift 4.12+
- CRI: containerd or CRI-O
- `tc`, `nsenter`, and `netem` kernel module available on nodes (`iproute2` and `util-linux` packages)
- Cluster-admin privileges (required to apply CRD and RBAC)
- (Optional) [Multus CNI](https://github.com/k8snetworkplumbingwg/multus-cni) for secondary interface injection

## Installation

### 1. Build the container image

```bash
make image IMAGE=<your-registry>/tc-injector:latest
docker push <your-registry>/tc-injector:latest
```

Update the `image:` field in `config/deploy/daemonset.yaml` to point to your registry.

### 2. Deploy (Kubernetes)

```bash
# Install the CRD, RBAC, and DaemonSet in one step
make deploy
```

Or apply each manifest separately:

```bash
kubectl apply -f config/crd/tcinjector.yaml
kubectl apply -f config/deploy/rbac.yaml
kubectl apply -f config/deploy/daemonset.yaml
```

### 3. Deploy (OpenShift)

OpenShift requires a SecurityContextConstraints (SCC) resource to grant the DaemonSet the `privileged` context it needs for `tc` and network namespace operations.

```bash
make deploy-openshift
```

### 4. Verify the DaemonSet is running

```bash
kubectl get daemonset -n tc-injector-system
kubectl get pods -n tc-injector-system
```

All pods should be in the `Running` state with `1/1` containers ready.

## Custom Resource Reference

### TCInjector

`TCInjector` is a cluster-scoped resource (`scope: Cluster`).

```yaml
apiVersion: tc-injector.setns.net/v1alpha1
kind: TCInjector
metadata:
  name: example-delay
spec:
  # rules is a list of delay injection rules.
  # The last rule whose selectors match a pod wins.
  rules:
    - selector:              # (required) Pod label selector
        matchLabels:
          app: backend
      namespaceSelector:     # (optional) Namespace label selector; empty = all namespaces
        matchLabels:
          env: production
      minDelay: 10           # (required) Minimum delay in milliseconds (>= 0)
      maxDelay: 50           # (required) Maximum delay in milliseconds (>= minDelay)
      multusNetworks:        # (optional) Multus NAD names to also inject delay into
        - default/mynetwork  # "namespace/name" (exact) or "name" (any namespace)
      injectPrimaryInterface: true  # (optional) set false to skip primary eth0/veth; default true

  # enablePeriodicDelayRotation re-randomizes delays within [minDelay, maxDelay]
  # at the interval specified by delayInterval.
  # Default: false
  enablePeriodicDelayRotation: false

  # delayInterval is the interval between delay re-randomizations.
  # Only takes effect when enablePeriodicDelayRotation is true.
  # Accepts Go duration strings: "30s", "1m", "2m30s". Default: "30s"
  delayInterval: "30s"
```

### Field Reference

| Field | Type | Required | Description |
|---|---|---|---|
| `spec.rules` | `[]DelayRule` | Yes | List of delay rules |
| `spec.rules[].selector` | `LabelSelector` | Yes | Matches pods by label |
| `spec.rules[].namespaceSelector` | `LabelSelector` | No | Matches pods by their namespace's labels. Empty matches all namespaces. |
| `spec.rules[].minDelay` | `int32` (ms) | Yes | Minimum delay (>= 0) |
| `spec.rules[].maxDelay` | `int32` (ms) | Yes | Maximum delay (>= minDelay) |
| `spec.rules[].multusNetworks` | `[]string` | No | Multus NAD names to inject delay into in addition to the primary interface. See [Multus interface targeting](#multus-interface-targeting). |
| `spec.rules[].injectPrimaryInterface` | `bool` | No | When `false`, skip tc delay injection on the pod's primary interface (`eth0`/`veth`). Use this to target only the interfaces listed in `multusNetworks`. Default: `true`. |
| `spec.enablePeriodicDelayRotation` | `bool` | No | Periodically re-randomize delays. Default: `false` |
| `spec.delayInterval` | `Duration` | No | Re-randomization interval. Default: `30s` |

### LabelSelector

Both `selector` and `namespaceSelector` accept the standard Kubernetes `LabelSelector` format:

```yaml
# Simple key=value matching
matchLabels:
  key: value

# Expression-based matching
matchExpressions:
  - key: environment
    operator: In          # In, NotIn, Exists, DoesNotExist
    values: [staging, canary]
```

## Examples

### Inject delay into all pods in a specific namespace

Label the target namespace first:

```bash
kubectl label namespace my-app env=production
```

Then create a `TCInjector`:

```yaml
apiVersion: tc-injector.setns.net/v1alpha1
kind: TCInjector
metadata:
  name: production-delay
spec:
  rules:
    - selector: {}           # matches all pods
      namespaceSelector:
        matchLabels:
          env: production
      minDelay: 20
      maxDelay: 80
```

### Inject delay into a specific service across all namespaces

```yaml
apiVersion: tc-injector.setns.net/v1alpha1
kind: TCInjector
metadata:
  name: backend-delay
spec:
  rules:
    - selector:
        matchLabels:
          app: backend
      minDelay: 50
      maxDelay: 150
```

### Multiple rules (last match wins)

```yaml
apiVersion: tc-injector.setns.net/v1alpha1
kind: TCInjector
metadata:
  name: multi-rule
spec:
  rules:
    # Apply 10-50 ms to all backend pods in production namespaces
    - selector:
        matchLabels:
          app: backend
      namespaceSelector:
        matchLabels:
          env: production
      minDelay: 10
      maxDelay: 50

    # Override: apply 200-500 ms to slow-test pods specifically
    - selector:
        matchLabels:
          app: backend
          test: slow
      namespaceSelector:
        matchLabels:
          env: production
      minDelay: 200
      maxDelay: 500
```

### Periodic delay rotation

```yaml
apiVersion: tc-injector.setns.net/v1alpha1
kind: TCInjector
metadata:
  name: jitter-test
spec:
  enablePeriodicDelayRotation: true
  delayInterval: "10s"
  rules:
    - selector:
        matchLabels:
          app: frontend
      minDelay: 10
      maxDelay: 200
```

### Multus interface targeting

Inject delay on both the primary interface (`eth0`) and a secondary interface attached by Multus (e.g. an ipvlan interface backed by the `mynetwork` NetworkAttachmentDefinition in the `default` namespace):

```yaml
apiVersion: tc-injector.setns.net/v1alpha1
kind: TCInjector
metadata:
  name: multus-delay
spec:
  rules:
    - selector:
        matchLabels:
          app: worker
      minDelay: 20
      maxDelay: 80
      multusNetworks:
        - default/mynetwork   # exact match: namespace/name
        # - mynetwork         # name-only: matches "mynetwork" in any namespace
```

To inject delay **only on the Multus interface** and leave the primary interface (`eth0`) unaffected, set `injectPrimaryInterface: false`:

```yaml
apiVersion: tc-injector.setns.net/v1alpha1
kind: TCInjector
metadata:
  name: multus-only-delay
spec:
  rules:
    - selector:
        matchLabels:
          app: multus-only
      minDelay: 100
      maxDelay: 200
      injectPrimaryInterface: false   # skip primary eth0/veth
      multusNetworks:
        - default/mynetwork
```

**How NAD names are matched** against the `k8s.v1.cni.cncf.io/network-status` annotation:

| Entry in `multusNetworks` | Matches annotation name |
|---|---|
| `default/mynetwork` | `default/mynetwork` (exact) |
| `mynetwork` | `default/mynetwork`, `kube-system/mynetwork`, `mynetwork`, etc. |

**Checking the applied rule** inside the pod network namespace:

```bash
# Find the target pod's PID on the node
kubectl exec -n tc-injector-system <tc-injector-pod> -- \
  cat /proc/$(pgrep -n -f <target-process>)/ns/net

# Or inspect via nsenter directly on the node
nsenter --net=/proc/<pid>/ns/net -- tc qdisc show dev net1
```

A successfully applied rule shows:

```
qdisc netem 1: root refcnt 2 limit 1000 delay 45ms
```

## Verifying That tc Rules Are Applied

### 1. Check the TCInjector status

```bash
kubectl get tcinjector <name> -o yaml
```

Look for `status.injectedPods` (number of pods currently receiving delay on each node) and `status.injectedPodDetails`:

```yaml
status:
  injectedPods: 2
  injectedPodDetails:
    # Pod with both primary and Multus interface injected (injectPrimaryInterface: true, default)
    - nodeName: worker-1
      namespace: default
      podName: worker-abc
      interface: eth0             # pod-side primary interface name
      delayMs: 37
      tcCommand: "nsenter --net=/proc/1234/ns/net -- tc qdisc replace dev eth0 root handle 1: netem delay 37ms"
      multusInterfaces:           # present when multusNetworks is specified
        - nadName: default/mynetwork
          interface: net1         # interface name inside the pod
          delayMs: 37
          tcCommand: "nsenter --net=/proc/1234/ns/net -- tc qdisc replace dev net1 root handle 1: netem delay 37ms"
    # Pod with Multus interface only (injectPrimaryInterface: false)
    - nodeName: worker-1
      namespace: default
      podName: multus-only-xyz
      interface: ""               # empty: primary interface was skipped
      delayMs: 150
      tcCommand: ""
      multusInterfaces:
        - nadName: default/mynetwork
          interface: net1
          delayMs: 150
          tcCommand: "nsenter --net=/proc/5678/ns/net -- tc qdisc replace dev net1 root handle 1: netem delay 150ms"
  conditions:
    - type: worker-1
      status: "True"
      message: "2 pod(s) injected on node worker-1"
```

### 2. Check the controller logs

```bash
kubectl logs -n tc-injector-system -l app=tc-injector -f
```

A successful injection looks like:

```
INFO  applying tc delay  pod=backend-xxx  iface=eth0  nad=primary  delayMs=37  tcCmd="nsenter --net=/proc/1234/ns/net -- tc qdisc replace dev eth0 root handle 1: netem delay 37ms"
```

### 3. Inspect the tc qdisc inside the pod

The exact tc command is recorded in `status.injectedPodDetails[].tcCommand`. You can verify the applied rule by running the same `nsenter` command from the DaemonSet pod on the target node:

```bash
# Find the tc-injector pod on the target node
kubectl get pods -n tc-injector-system -o wide

# Get the PID of a process inside the target pod (from the status tcCommand or manually)
kubectl get tcinjector <name> -o jsonpath='{.status.injectedPodDetails[*].tcCommand}'

# Exec into the tc-injector DaemonSet pod and run nsenter
kubectl exec -it -n tc-injector-system <tc-injector-pod> -- \
  nsenter --net=/proc/<pid>/ns/net -- tc qdisc show dev eth0
```

A successfully applied rule shows:

```
qdisc netem 1: root refcnt 2 limit 1000 delay 37ms
```

If there is no qdisc or the output is `qdisc noqueue ...`, the rule has not been applied.

### 4. Measure actual latency from inside the pod

```bash
kubectl exec -it <target-pod> -- bash

# Ping another pod or service to measure RTT
ping -c 10 <other-pod-ip>

# Or measure HTTP response time with curl
curl -o /dev/null -s -w "time_total: %{time_total}s\n" http://<service>/
```

The measured RTT increase should correspond to the injected delay (`minDelay`-`maxDelay` ms).

### 5. Troubleshooting checklist

| Symptom | Likely cause | Action |
|---|---|---|
| `injectedPods: 0` | Selector mismatch | Check pod and namespace labels match the rule's selectors |
| No `applying tc delay` in logs | Pod not ready | Ensure pod is `Running` with all containers ready |
| Log shows injection but no measured delay | tc applied, but direction mismatch | tc targets **egress** (packets leaving the pod); verify with `nsenter` (see step 3) |
| Pod is not targeted despite matching labels | Namespace labels missing | Run `kubectl get namespace <ns> --show-labels` and add required labels |
| Multus interface not injected (`multusInterfaces` empty in status) | Annotation missing or NAD name mismatch | Check the pod annotation and NAD name format (see below) |
| Log shows `cannot find netns path` | CRI lookup failed for container | Check DaemonSet logs; ensure the CRI socket is correctly mounted |
| Primary interface delay unexpectedly absent | `injectPrimaryInterface: false` set in rule | Intentional when targeting only Multus interfaces; set to `true` or omit to restore primary injection |

```bash
# Debug selector matching
kubectl get pod <target-pod> -o jsonpath='{.metadata.labels}'
kubectl get namespace <target-namespace> --show-labels
kubectl get tcinjector <name> -o jsonpath='{.spec.rules}'

# Debug Multus interface resolution
kubectl get pod <target-pod> -o jsonpath='{.metadata.annotations.k8s\.v1\.cni\.cncf\.io/network-status}' | jq .
```

The `network-status` annotation lists all attached interfaces. Confirm the `name` field matches the entry in `multusNetworks`. For example, if the annotation shows `"name": "default/mynetwork"`, the rule should specify `multusNetworks: [default/mynetwork]` or `multusNetworks: [mynetwork]`.

## Uninstallation

```bash
# Remove DaemonSet and RBAC (keeps CRD and any TCInjector resources)
make undeploy

# Remove the CRD (also deletes all TCInjector resources)
make uninstall-crd

# OpenShift: also remove the SCC
make undeploy-openshift
make uninstall-scc
```

## Development

```bash
# Run unit tests
make test

# Run tests with race detector
make test-race

# Build binary locally
make build
```
