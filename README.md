# k8sible

A Kubernetes Operator for running Ansible playbooks via GitOps.

k8sible enables declarative infrastructure automation by watching Git repositories for Ansible playbooks and executing them automatically when changes are detected or on a schedule.

## Features

- **GitOps-driven**: Automatically detects new commits and triggers playbook execution
- **Scheduled execution**: Run playbooks on cron schedules
- **Apply/Reconcile pattern**: Separate playbooks for initial apply and drift correction
- **Automatic retry**: Configurable retry logic with failure cooldown periods
- **Secure**: Supports private repositories via Git tokens, runs as non-root
- **Observable**: Kubernetes-native status tracking and events

## Architecture

```
┌─────────────────────────────────────────────────────────────┐
│                    Kubernetes Cluster                        │
│  ┌───────────────────┐       ┌────────────────────────────┐ │
│  │  k8sible Operator │       │    K8sibleWorkflow CR      │ │
│  │                   │──────▶│  - source.repository       │ │
│  │  - Watch CRs      │       │  - apply.path              │ │
│  │  - Monitor Git    │       │  - reconcile.path          │ │
│  │  - Create Jobs    │       │  - schedule                │ │
│  └───────────────────┘       └────────────────────────────┘ │
│           │                                                  │
│           ▼                                                  │
│  ┌───────────────────┐       ┌────────────────────────────┐ │
│  │  Kubernetes Job   │       │     Git Repository         │ │
│  │  (ansible-runner) │◀──────│  - Ansible playbooks       │ │
│  │                   │       │  - Inventory files         │ │
│  └───────────────────┘       └────────────────────────────┘ │
└─────────────────────────────────────────────────────────────┘
```

## Installation

### Prerequisites

- Kubernetes cluster v1.26+
- Helm 3.x (for Helm installation)
- kubectl configured to access your cluster

### Install with Helm

```bash
# Install from local chart
helm install k8sible ./charts/k8sible \
  --namespace k8sible \
  --create-namespace
```

#### Helm Configuration Options

| Parameter | Description | Default |
|-----------|-------------|---------|
| `replicaCount` | Number of operator replicas | `1` |
| `image.repository` | Operator image repository | `ghcr.io/bensonphillipsiv/k8sible` |
| `image.tag` | Operator image tag | `latest` |
| `image.pullPolicy` | Image pull policy | `IfNotPresent` |
| `leaderElection.enabled` | Enable leader election for HA | `true` |
| `metrics.enabled` | Enable metrics endpoint | `true` |
| `metrics.service.port` | Metrics service port | `8443` |
| `resources.requests.cpu` | CPU request | `10m` |
| `resources.requests.memory` | Memory request | `64Mi` |
| `resources.limits.cpu` | CPU limit | `500m` |
| `resources.limits.memory` | Memory limit | `128Mi` |
| `crds.install` | Install CRDs with chart | `true` |
| `crds.keep` | Keep CRDs on uninstall | `true` |

Example with custom values:

```bash
helm install k8sible ./charts/k8sible \
  --namespace k8sible \
  --create-namespace \
  --set replicaCount=2 \
  --set leaderElection.enabled=true \
  --set resources.requests.memory=128Mi
```

## Usage

### Basic Example

1. Create a secret for Git authentication (optional, for private repos):

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: git-credentials
  namespace: k8sible
type: Opaque
stringData:
  token: ghp_your_github_token_here
```

2. Create a K8sibleWorkflow resource:

```yaml
apiVersion: k8sible.core.k8sible.io/v1alpha1
kind: K8sibleWorkflow
metadata:
  name: my-infrastructure
  namespace: k8sible
spec:
  source:
    repository: https://github.com/your-org/ansible-playbooks
    reference: main
    secretRef:
      name: git-credentials
  apply:
    path: playbooks/deploy.yaml
    maxRetries: 3
  reconcile:
    path: playbooks/reconcile.yaml
    schedule: "0 */6 * * *"  # Every 6 hours
    maxRetries: 3
  failureCycleCooldown: 30m
```

3. Apply the resource:

```bash
kubectl apply -f my-workflow.yaml
```

4. Monitor the workflow:

```bash
# View workflow status
kubectl get k8sibleworkflows -n k8sible

# Watch for changes
kubectl get k8sibleworkflows -n k8sible -w

# Detailed status
kubectl describe k8sibleworkflow my-infrastructure -n k8sible
```

### With Environment Variables

Pass environment variables to your Ansible playbooks using secrets or configmaps:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: ansible-vars
  namespace: k8sible
type: Opaque
stringData:
  AWS_ACCESS_KEY_ID: "AKIA..."
  AWS_SECRET_ACCESS_KEY: "..."
---
apiVersion: k8sible.core.k8sible.io/v1alpha1
kind: K8sibleWorkflow
metadata:
  name: aws-infrastructure
  namespace: k8sible
spec:
  source:
    repository: https://github.com/your-org/aws-ansible
    reference: main
  apply:
    path: playbooks/provision.yaml
  secretRef:
    name: ansible-vars
```

### With Custom Inventory

Use `inventoryPath` to specify an inventory file. Environment variable substitution is supported via `envsubst`:

```yaml
apiVersion: k8sible.core.k8sible.io/v1alpha1
kind: K8sibleWorkflow
metadata:
  name: multi-host-config
  namespace: k8sible
spec:
  source:
    repository: https://github.com/your-org/ansible-config
    reference: main
  apply:
    path: playbooks/configure.yaml
  inventoryPath: inventory/production.ini
  secretRef:
    name: host-credentials
```

Your inventory file can use environment variables:

```ini
[webservers]
${WEB_HOST_1}
${WEB_HOST_2}

[databases]
${DB_HOST}
```

### Scheduled Runs

Use cron expressions for scheduled playbook execution:

```yaml
spec:
  apply:
    path: playbooks/deploy.yaml
    schedule: "0 2 * * *"      # Daily at 2 AM
  reconcile:
    path: playbooks/check.yaml
    schedule: "*/15 * * * *"   # Every 15 minutes
```

### Verbosity Levels

Control Ansible output verbosity (0-4):

```yaml
spec:
  verbosity: 2  # Equivalent to ansible-playbook -vv
```

| Level | Ansible Flag | Description |
|-------|--------------|-------------|
| 0 | (none) | Normal output |
| 1 | -v | Verbose |
| 2 | -vv | More verbose |
| 3 | -vvv | Debug |
| 4 | -vvvv | Connection debug |

## CRD Reference

### K8sibleWorkflow Spec

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `source.repository` | string | Yes | Git repository URL |
| `source.reference` | string | No | Git branch/tag/commit (default: main) |
| `source.secretRef.name` | string | No | Secret containing Git token |
| `source.secretRef.key` | string | No | Key in secret (default: token) |
| `apply.path` | string | Yes | Path to apply playbook |
| `apply.schedule` | string | No | Cron schedule for apply |
| `apply.maxRetries` | int | No | Max retries (0-10, default: 3) |
| `reconcile.path` | string | No | Path to reconcile playbook |
| `reconcile.schedule` | string | No | Cron schedule for reconcile |
| `reconcile.maxRetries` | int | No | Max retries (0-10, default: 3) |
| `failureCycleCooldown` | string | No | Delay before retry (e.g., "30m") |
| `secretRef.name` | string | No | Secret for Ansible env vars |
| `configMapRef.name` | string | No | ConfigMap for Ansible env vars |
| `inventoryPath` | string | No | Path to inventory file |
| `verbosity` | int | No | Ansible verbosity (0-4, default: 0) |

### K8sibleWorkflow Status

| Field | Description |
|-------|-------------|
| `applyCommit` | Latest detected commit for apply playbook |
| `reconcileCommit` | Latest detected commit for reconcile playbook |
| `pendingPlaybooks` | Queue of playbooks waiting to run |
| `lastTriggerReason` | Why playbooks were queued (new_commit, schedule, failure_retry) |
| `lastSuccessfulRun` | Details of last successful playbook run |
| `lastFailedRun` | Details of last failed playbook run |
| `applyScheduleStatus` | Last scheduled apply run time |
| `reconcileScheduleStatus` | Last scheduled reconcile run time |
| `conditions` | Standard Kubernetes conditions |

## How It Works

### Execution Flow

1. **Commit Detection**: The operator polls GitHub API every 3 minutes for new commits affecting your playbook paths
2. **Job Creation**: When a new commit is detected or a schedule triggers, a Kubernetes Job is created
3. **Playbook Execution**: The ansible-runner container clones the repo and executes the playbook
4. **Status Update**: Success/failure is recorded in the K8sibleWorkflow status

### Apply vs Reconcile

- **Apply**: Main playbook for provisioning/configuration changes
- **Reconcile**: Validation playbook to detect and correct drift

When apply runs successfully, reconcile is automatically queued afterward. If reconcile fails (drift detected), apply is re-triggered to correct the state.

### Failure Handling

- Failed playbooks are automatically retried up to `maxRetries` times
- `failureCycleCooldown` prevents retry storms by adding a delay between attempts
- New commits always trigger runs immediately (ignoring cooldown)

## Development

### Prerequisites

- Go 1.24+
- Docker
- kubectl
- Access to a Kubernetes cluster

### Build

```bash
# Build the operator binary
make build

# Build Docker image
make docker-build IMG=k8sible:dev

# Run tests
make test

# Generate manifests
make manifests
```

### Local Development

```bash
# Install CRDs
make install

# Run the operator locally
make run
```

### Project Distribution

#### YAML Bundle

Build an installer with all YAML files:

```bash
make build-installer IMG=<some-registry>/k8sible:tag
```

Users can install with:

```bash
kubectl apply -f https://raw.githubusercontent.com/bensonphillipsiv/k8sible/<tag>/dist/install.yaml
```

## Contributing

Contributions are welcome! Please feel free to submit issues and pull requests.

Run `make help` for information on all available make targets.

## License

Copyright 2025 bensonphillipsiv.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
