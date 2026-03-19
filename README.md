# configctl

CLI tool to stop AWS Config configuration recorders across an AWS Organization.

## Prerequisites

- Go 1.23+
- AWS credentials with permissions to:
  - `organizations:DescribeOrganization` (for management account detection)
  - `sts:AssumeRole` on the configured role in target accounts
- The assumed role (e.g. `AWSControlTowerExecution`) must have:
  - `config:DescribeConfigurationRecorders`
  - `config:DescribeConfigurationRecorderStatus`
  - `config:StopConfigurationRecorder`

## Build

```bash
go build -o configctl .
```

## Configuration

Create a `config.yaml` (see `config.example.yaml`):

```yaml
accounts:
  - "111111111111"
  - "222222222222"

regions:
  - us-east-1
  - us-west-2
  - eu-west-1

assumeRole: AWSControlTowerExecution
awsProfile: mgmt-prod

concurrency:
  maxWorkers: 10
  retryMaxAttempts: 5
  retryMaxBackoff: 30s
```

| Field | Description |
|---|---|
| `accounts` | List of target AWS account IDs |
| `regions` | Regions to check in every account |
| `assumeRole` | IAM role name to assume via STS |
| `awsProfile` | AWS CLI profile name (e.g. SSO profile for management account) |
| `concurrency.maxWorkers` | Max concurrent account×region operations |
| `concurrency.retryMaxAttempts` | SDK retry ceiling for throttled/transient errors |
| `concurrency.retryMaxBackoff` | Max backoff between retries (Go duration, e.g. `30s`) |

## Usage

### Dry run — check current state

```bash
aws sso login --profile mgmt-prod
configctl disable --config config.yaml --dry-run --output report.json
```

Reports which recorders are enabled/disabled/absent without making changes.

### Live run — stop recorders

```bash
configctl disable --config config.yaml --output report.json
```

Stops all active Config recorders. Does **not** delete recorders or delivery channels.

### Flags

| Flag | Short | Default | Description |
|---|---|---|---|
| `--config` | `-c` | `config.yaml` | Path to YAML config |
| `--dry-run` | `-d` | `false` | Check only, no changes |
| `--output` | `-o` | `report.json` | Path for JSON report |
| `--aws-profile` | `-p` | | AWS CLI profile (overrides config `awsProfile`) |

## Output

The JSON report enumerates both customer-managed and service-linked recorders per region:

```json
{
  "runMode": "dry-run",
  "timestamp": "2026-03-19T14:30:00Z",
  "summary": {
    "totalAccounts": 3,
    "totalRegions": 10,
    "customerRecordersEnabled": 28,
    "customerRecordersDisabled": 2,
    "customerRecordersAbsent": 0,
    "serviceLinkedRecorders": 5,
    "serviceLinkedRecordersPaid": 1,
    "errors": 0
  },
  "accounts": [
    {
      "accountId": "111111111111",
      "isManagementAccount": true,
      "regions": [
        {
          "region": "us-east-1",
          "customerRecorder": {
            "name": "default",
            "wasEnabled": true,
            "currentStatus": "enabled"
          },
          "serviceLinkedRecorders": [
            {
              "name": "AWSConfigurationRecorderForCloudWatch",
              "wasEnabled": true,
              "currentStatus": "enabled",
              "recordingScope": "INTERNAL",
              "servicePrincipal": "cloudwatch.amazonaws.com"
            }
          ]
        }
      ]
    }
  ]
}
```

**Customer-managed recorder** (`customerRecorder`):
- `null` if no customer-managed recorder exists in that region
- `wasEnabled` / `currentStatus` track the before/after state
- Only customer-managed recorders are stopped during live runs

**Service-linked recorders** (`serviceLinkedRecorders`):
- Created by AWS services (Security Hub, CloudWatch, etc.) — identified by the `AWSConfigurationRecorderFor` name prefix
- `servicePrincipal` shows which AWS service owns the recorder
- `recordingScope`: `INTERNAL` = free, no CIs in delivery channel; `PAID` = generates billable CIs
- **Cannot be stopped via the Config API** — must be disabled through the owning AWS service
- The tool reports them for visibility but never attempts to stop them

## Notes

- The tool assumes the configured role from the management account. Run this with management account credentials.
- Management accounts are auto-detected and flagged in the report. SCPs do not protect the management account — handle it separately if needed.
- The SDK's built-in exponential backoff with jitter handles rate limiting. The `retryMaxAttempts` and `retryMaxBackoff` settings control the ceiling.
- Only `StopConfigurationRecorder` is called. Recorders, delivery channels, and recorded data are preserved.

## SCPs — Prevent Config Re-enablement

Two SCPs are included. Apply them to your target OUs **before** running the tool.

### `scp-deny-config-customer-recorder.json` (deploy this)

Blocks re-enablement of customer-managed recorders. Denies:

- `config:StartConfigurationRecorder`
- `config:PutConfigurationRecorder`
- `config:PutDeliveryChannel`
- `config:DeleteConfigurationRecorder` (prevents delete-and-recreate workaround)

This is the primary SCP. It blocks Control Tower's `AWSControlTowerBP-BASELINE-CONFIG` StackSet, Firewall Manager's prerequisite StackSet, and any manual re-enablement. A break-glass role is exempted — edit the ARN to match your environment, or remove the condition for an absolute deny.

### `scp-deny-config-service-linked-recorder.json` (deploy with caution)

Blocks creation of service-linked recorders. Denies:

- `config:PutServiceLinkedConfigurationRecorder`

**Warning:** This will break AWS services that create service-linked recorders (CloudWatch telemetry audit, Security Hub features, etc.). Only deploy this if you've confirmed no services in your org depend on service-linked recorders, or if the cost savings justify the feature loss. Service-linked recorders with `INTERNAL` recording scope are free — run a dry-run first and check the `serviceLinkedRecordersPaid` count in the report before deciding.

### Reminders

- **SCPs do not apply to the management account.** Handle it separately.
- **Control Tower will flag drift** when its StackSet operations fail against the SCP. This is expected noise — CT can't fix it, but it will complain.
- **Firewall Manager requires Config** for compliance monitoring. If you're using FMS, check for its prerequisite StackSet (`aws-fms-*`) in the management account and either delete it or set `EnableConfig=No`.

### Sources of Config re-enablement to audit

| Source | Mechanism | Blocked by SCP? |
|---|---|---|
| Control Tower baseline | `AWSControlTowerBP-BASELINE-CONFIG` StackSet | Yes (customer SCP) |
| Firewall Manager prereq | `aws-fms-prereq` StackSet with `EnableConfig=Yes` | Yes (customer SCP) |
| Security Hub | Creates service-linked Config rules (not recorders) | N/A |
| CloudWatch telemetry | Service-linked recorder (`INTERNAL` scope, free) | Only with service-linked SCP |
| Manual re-enablement | Console or API call | Yes (customer SCP) |
| Systems Manager Quick Setup | Can deploy Config recorders via StackSets | Yes (customer SCP) |
