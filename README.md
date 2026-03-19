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

| Field                          | Description                                                    |
| ------------------------------ | -------------------------------------------------------------- |
| `accounts`                     | List of target AWS account IDs                                 |
| `regions`                      | Regions to check in every account                              |
| `assumeRole`                   | IAM role name to assume via STS                                |
| `awsProfile`                   | AWS CLI profile name (e.g. SSO profile for management account) |
| `concurrency.maxWorkers`       | Max concurrent account×region operations                       |
| `concurrency.retryMaxAttempts` | SDK retry ceiling for throttled/transient errors               |
| `concurrency.retryMaxBackoff`  | Max backoff between retries (Go duration, e.g. `30s`)          |

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

| Flag            | Short | Default       | Description                                     |
| --------------- | ----- | ------------- | ----------------------------------------------- |
| `--config`      | `-c`  | `config.yaml` | Path to YAML config                             |
| `--dry-run`     | `-d`  | `false`       | Check only, no changes                          |
| `--output`      | `-o`  | `report.json` | Path for JSON report                            |
| `--aws-profile` | `-p`  |               | AWS CLI profile (overrides config `awsProfile`) |

## Output

The JSON report contains:

```json
{
  "runMode": "dry-run",
  "timestamp": "2026-03-19T14:30:00Z",
  "summary": {
    "totalAccounts": 3,
    "totalRegions": 10,
    "recordersEnabled": 28,
    "recordersDisabled": 2,
    "recordersAbsent": 0,
    "errors": 0
  },
  "accounts": [
    {
      "accountId": "111111111111",
      "isManagementAccount": true,
      "regions": [
        {
          "region": "us-east-1",
          "recorderName": "default",
          "wasEnabled": true,
          "currentStatus": "enabled",
          "error": null
        }
      ]
    }
  ]
}
```

- `wasEnabled`: recorder state **before** the run
- `currentStatus`: recorder state **after** the run (`enabled`, `disabled`, `stopped`, `absent`, `error`)
- `isManagementAccount`: flagged accounts where SCPs do **not** apply

## Notes

- The tool assumes the configured role from the management account. Run this with management account credentials.
- Management accounts are auto-detected and flagged in the report. SCPs do not protect the management account — handle it separately if needed.
- The SDK's built-in exponential backoff with jitter handles rate limiting. The `retryMaxAttempts` and `retryMaxBackoff` settings control the ceiling.
- Only `StopConfigurationRecorder` is called. Recorders, delivery channels, and recorded data are preserved.

## SCP — Prevent Config Re-enablement

Before running the tool, attach `scp-deny-config-recorder.json` to your target OUs to prevent Config from being turned back on (by Control Tower drift remediation or anything else).

The included SCP denies `StartConfigurationRecorder`, `PutConfigurationRecorder`, and `PutDeliveryChannel` for all principals except a break-glass role. Edit the `BreakGlassRole` ARN to match your environment, or remove the condition block entirely for an absolute deny.

**Reminder:** SCPs do not apply to the management account. The management account must be monitored separately (see the `isManagementAccount` flag in reports).
