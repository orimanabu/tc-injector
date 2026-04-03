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
┌──────────────────────────────────────────────────────────┐
│  Kubernetes Node                                         │
│                                                          │
│  ┌──────────────┐     watches      ┌──────────────────┐  │
│  │  tc-injector │ ←─────────────── │  TCInjector CRD  │  │
│  │  (DaemonSet) │                  └──────────────────┘  │
│  └──────┬───────┘                                        │
│         │  nsenter --net=/proc/<pid>/ns/net              │
│         ▼                                                │
│  ┌─────────────────────────────────────────────────────┐ │
│  │  pod network namespace                              │ │
│  │                                                     │ │
│  │  eth0 ──[netem delay 30ms]──► outbound packets      │ │
│  │  net1 ──[netem delay 30ms]──► (Multus, if requested)│ │
│  └─────────────────────────────────────────────────────┘ │
└──────────────────────────────────────────────────────────┘
```

> **Direction**: tc delay is applied to **egress** traffic leaving the pod (outgoing packets).
> This is the opposite of applying tc to the host-side veth peer, which would delay ingress.
> For round-trip latency measurement (e.g. `ping`) the direction does not affect the observed RTT.

### Multus Interface Support

When a rule specifies `multusNetworks`, tc-injector also injects delay into interfaces added by [Multus CNI](https://github.com/k8snetworkplumbingwg/multus-cni) (e.g. ipvlan secondary interfaces). 

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
podman push <your-registry>/tc-injector:latest
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
      target:                # (optional) which interfaces to inject delay into
        # primary: true      # (optional) set false to skip primary eth0; default true
        multusNetworks:      # (optional) Multus NAD names to also inject delay into
          - default/mynetwork  # "namespace/name" (exact) or "name" (any namespace)

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
| `spec.rules[].target` | `Target` | No | Specifies which interfaces inside the pod receive delay injection. If omitted, only the primary interface (`eth0`) is targeted. |
| `spec.rules[].target.primary` | `bool` | No | When `false`, skip tc delay injection on the pod's primary interface (`eth0`). Use this to target only the interfaces listed in `multusNetworks`. Default: `true`. |
| `spec.rules[].target.multusNetworks` | `[]string` | No | Multus NAD names to inject delay into. See [Multus interface targeting](#multus-interface-targeting). |
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
      target:
        multusNetworks:
          - default/mynetwork   # exact match: namespace/name
          # - mynetwork         # name-only: matches "mynetwork" in any namespace
```

To inject delay **only on the Multus interface** and leave the primary interface (`eth0`) unaffected, set `target.primary: false`:

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
      target:
        primary: false          # skip primary eth0
        multusNetworks:
          - default/mynetwork
```

**How NAD names are matched** against the `k8s.v1.cni.cncf.io/network-status` annotation:

| Entry in `target.multusNetworks` | Matches annotation name |
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
    # Pod with both primary and Multus interface injected (target.primary: true, default)
    - nodeName: worker-1
      namespace: default
      podName: worker-abc
      interface: eth0             # pod-side primary interface name
      delayMs: 37
      tcCommand: "nsenter --net=/proc/1234/ns/net -- tc qdisc replace dev eth0 root handle 1: netem delay 37ms"
      multusInterfaces:           # present when target.multusNetworks is specified
        - nadName: default/mynetwork
          interface: net1         # interface name inside the pod
          delayMs: 37
          tcCommand: "nsenter --net=/proc/1234/ns/net -- tc qdisc replace dev net1 root handle 1: netem delay 37ms"
    # Pod with Multus interface only (target.primary: false)
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

Suppose the TCInjector custom resource is configured as follows:

```
apiVersion: tc-injector.setns.net/v1alpha1
kind: TCInjector
metadata:
  name: example-delay
spec:
  rules:
    - selector:                    
        matchLabels:                        # This rule applies to pods labeled `app=multus-ipvlan`
          app: multus-ipvlan                # in the `tmp` namespace.
      namespaceSelector:                    #
        matchLabels:                        #
          kubernetes.io/metadata.name: tmp  #
      minDelay: 600           # Set a 600ms–700ms latency
      maxDelay: 700           #
      target:
        primary: false        # Excluded from the pod's primary interface (eth0).
        multusNetworks:       # Set latency on additional interfaces connected
          - tmp/nad-ipvlan-1  # to `nad-ipvlan-1` in the `tmp` namespace.
```

Checking the .status of TCInjector shows that a 699ms latency has been applied to pod pod-with-delay.

```
$ kubectl get tcinjector example-delay -o yaml | yq '.status.injectedPodDetails.[] | select(.podName | contains("pod-with"))'
delayMs: 699
interface: ""
multusInterfaces:
  - delayMs: 699
    interface: net1
    nadName: tmp/nad-ipvlan-1
    tcCommand: 'nsenter --net=/proc/1313495/ns/net -- tc qdisc replace dev net1 root handle 1: netem delay 699ms'
namespace: tmp
nodeName: wk3
podName: pod-with-delay
tcCommand: ""
```

Inspect the interfaces of pod-without-delay (no latency) and pod-with-delay (with latency).
This confirms that `qdisc netem` is applied to net1 of pod-with-latency.

- pod-without-latency
```
$ kubectl exec pod-without-delay -- ip addr show
1: lo: <LOOPBACK,UP,LOWER_UP> mtu 65536 qdisc noqueue state UNKNOWN group default qlen 1000
    link/loopback 00:00:00:00:00:00 brd 00:00:00:00:00:00
    inet 127.0.0.1/8 scope host lo
       valid_lft forever preferred_lft forever
    inet6 ::1/128 scope host
       valid_lft forever preferred_lft forever
2: eth0@if5096: <BROADCAST,MULTICAST,UP,LOWER_UP> mtu 1400 qdisc noqueue state UP group default
    link/ether 0a:58:0a:80:06:0f brd ff:ff:ff:ff:ff:ff link-netnsid 0
    inet 10.128.6.15/24 brd 10.128.6.255 scope global eth0
       valid_lft forever preferred_lft forever
    inet6 fe80::858:aff:fe80:60f/64 scope link
       valid_lft forever preferred_lft forever
3: net1@if3: <BROADCAST,MULTICAST,UP,LOWER_UP> mtu 1500 qdisc noqueue state UNKNOWN group default qlen 1000
    link/ether 52:54:00:b3:5a:e9 brd ff:ff:ff:ff:ff:ff link-netnsid 0
    inet 172.17.20.10/24 brd 172.17.20.255 scope global net1
       valid_lft forever preferred_lft forever
    inet6 fe80::5254:0:3b3:5ae9/64 scope link
       valid_lft forever preferred_lft forever
```

- pod-with-latency
```
$ kubectl exec pod-with-delay -- ip addr show
1: lo: <LOOPBACK,UP,LOWER_UP> mtu 65536 qdisc noqueue state UNKNOWN group default qlen 1000
    link/loopback 00:00:00:00:00:00 brd 00:00:00:00:00:00
    inet 127.0.0.1/8 scope host lo
       valid_lft forever preferred_lft forever
    inet6 ::1/128 scope host
       valid_lft forever preferred_lft forever
2: eth0@if4496: <BROADCAST,MULTICAST,UP,LOWER_UP> mtu 1400 qdisc noqueue state UP group default
    link/ether 0a:58:0a:80:07:35 brd ff:ff:ff:ff:ff:ff link-netnsid 0
    inet 10.128.7.53/24 brd 10.128.7.255 scope global eth0
       valid_lft forever preferred_lft forever
    inet6 fe80::858:aff:fe80:735/64 scope link
       valid_lft forever preferred_lft forever
3: net1@if3: <BROADCAST,MULTICAST,UP,LOWER_UP> mtu 1500 qdisc netem state UNKNOWN group default qlen 1000
    link/ether 52:54:00:73:ce:05 brd ff:ff:ff:ff:ff:ff link-netnsid 0
    inet 172.17.20.11/24 brd 172.17.20.255 scope global net1
       valid_lft forever preferred_lft forever
    inet6 fe80::5254:0:273:ce05/64 scope link
       valid_lft forever preferred_lft forever
```

Ping results via net1 reflect the applied latency.

```
$ kubectl exec pod-with-delay -- ping -c 5 172.17.20.10
PING 172.17.20.10 (172.17.20.10) 56(84) bytes of data.
64 bytes from 172.17.20.10: icmp_seq=1 ttl=64 time=1399 ms
64 bytes from 172.17.20.10: icmp_seq=2 ttl=64 time=699 ms
64 bytes from 172.17.20.10: icmp_seq=3 ttl=64 time=699 ms
64 bytes from 172.17.20.10: icmp_seq=4 ttl=64 time=699 ms
64 bytes from 172.17.20.10: icmp_seq=5 ttl=64 time=699 ms
```

### 5. Troubleshooting checklist

| Symptom | Likely cause | Action |
|---|---|---|
| `injectedPods: 0` | Selector mismatch | Check pod and namespace labels match the rule's selectors |
| No `applying tc delay` in logs | Pod not ready | Ensure pod is `Running` with all containers ready |
| Log shows injection but no measured delay | tc applied, but direction mismatch | tc targets **egress** (packets leaving the pod); verify with `nsenter` (see step 3) |
| Pod is not targeted despite matching labels | Namespace labels missing | Run `kubectl get namespace <ns> --show-labels` and add required labels |
| Multus interface not injected (`multusInterfaces` empty in status) | Annotation missing or NAD name mismatch | Check the pod annotation and NAD name format (see below) |
| Log shows `cannot find netns path` | CRI lookup failed for container | Check DaemonSet logs; ensure the CRI socket is correctly mounted |
| Primary interface delay unexpectedly absent | `target.primary: false` set in rule | Intentional when targeting only Multus interfaces; set `target.primary: true` or omit `target` to restore primary injection |

```bash
# Debug selector matching
kubectl get pod <target-pod> -o jsonpath='{.metadata.labels}'
kubectl get namespace <target-namespace> --show-labels
kubectl get tcinjector <name> -o jsonpath='{.spec.rules}'

# Debug Multus interface resolution
kubectl get pod <target-pod> -o jsonpath='{.metadata.annotations.k8s\.v1\.cni\.cncf\.io/network-status}' | jq .
```

The `network-status` annotation lists all attached interfaces. Confirm the `name` field matches the entry in `target.multusNetworks`. For example, if the annotation shows `"name": "default/mynetwork"`, the rule should specify `target.multusNetworks: [default/mynetwork]` or `target.multusNetworks: [mynetwork]`.

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
