# GitHub Actions Workflows

This directory contains automated workflows for the grpc-go-shmem project.

## Benchmarks Workflow

**File:** `benchmarks.yml`

### Purpose
Runs performance benchmarks for the shared memory transport implementation in the cloud and provides downloadable results.

### Triggers
- **Manual**: Navigate to Actions → Benchmarks → "Run workflow" button
- **Scheduled**: Automatically runs weekly on Sundays at midnight UTC
- **On Push** (optional): Can be enabled by uncommenting the push trigger in the workflow file

### What It Does
1. Sets up a clean Ubuntu environment with Go 1.25 and Python 3.11
2. Installs required dependencies (matplotlib, numpy)
3. Executes the full benchmark suite using `benchmark_runner.py --run`
4. Generates performance comparison plots
5. Uploads results as artifacts (retained for 90 days)
6. Displays a summary in the workflow logs
7. Validates that benchmarks completed successfully

### Benchmark Results
The workflow produces:
- `benchmark_results.json` - Structured benchmark data
- `benchmark_results.txt` - Raw Go benchmark output
- `*.png` - Performance comparison plots

### Downloading Results
1. Go to the Actions tab in GitHub
2. Click on the completed "Benchmarks" workflow run
3. Scroll to the "Artifacts" section at the bottom
4. Download `benchmark-results-<run-id>.zip`

### Viewing Results
After downloading and extracting the artifact:
- Open any `.png` file to view performance graphs
- Review `benchmark_results.txt` for detailed Go benchmark output
- Parse `benchmark_results.json` for programmatic analysis

### Expected Runtime
- Typical run time: 10-15 minutes
- Timeout: 30 minutes maximum

### Platform
- Runs on: `ubuntu-latest` (currently Ubuntu 22.04)
- Go version: 1.25
- Python version: 3.11

## Other Workflows

- **testing.yml** - Main test suite
- **pr-validation.yml** - Pull request checks  
- **coverage.yml** - Code coverage reporting
- **codeql-analysis.yml** - Security analysis
- **deps.yml** - Dependency updates
- **release.yml** - Release automation
- **stale.yml** - Stale issue management
- **lock.yml** - Thread locking

See individual workflow files for more details.
