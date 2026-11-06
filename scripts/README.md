# Local scripts

- `generate-cloud-init.ps1`: inserts the local SSH **public** key into ignored `infra/generated/cloud-init.yaml`. It does not contact Hetzner.
- `build-bridge.ps1`: runs the focused Go device tests and builds an ignored Windows binary.
- `test-cg6-endurance.ps1`: prompts for lab credentials, tests an owned camera, writes ignored results under `local/lab-results/`, and deletes the cached auth token afterward. It requires the built bridge binary.

Run scripts from PowerShell 7 with `pwsh -NoProfile -File scripts/<name>.ps1`. Do not pass account passwords as command-line arguments.
