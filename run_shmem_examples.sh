#!/bin/bash
set -o pipefail  # Make pipelines return the exit status of the last command to fail

# Run all shared memory gRPC example programs and record output

LOG_FILE="/workspaces/grpc-go-shmem/shmem_examples_output.log"
EXAMPLES_DIR="/workspaces/grpc-go-shmem/examples"

echo "=========================================" | tee "$LOG_FILE"
echo "Shared Memory gRPC Examples - Test Run" | tee -a "$LOG_FILE"
echo "Date: $(date)" | tee -a "$LOG_FILE"
echo "=========================================" | tee -a "$LOG_FILE"
echo "" | tee -a "$LOG_FILE"

cd "$EXAMPLES_DIR"

# Clean up any leftover shared memory segments
rm -f /dev/shm/helloworld_shm /dev/shm/routeguide_shm /dev/shm/my_segment 2>/dev/null

# Track success/failure
PASSED=0
FAILED=0

run_test() {
    local name="$1"
    shift
    echo "=========================================" | tee -a "$LOG_FILE"
    echo "Running: $name" | tee -a "$LOG_FILE"
    echo "=========================================" | tee -a "$LOG_FILE"
    
    if "$@" 2>&1 | tee -a "$LOG_FILE"; then
        echo "" | tee -a "$LOG_FILE"
        echo "✅ $name: SUCCESS" | tee -a "$LOG_FILE"
        echo "" | tee -a "$LOG_FILE"
        ((PASSED++))
        return 0
    else
        echo "" | tee -a "$LOG_FILE"
        echo "❌ $name: FAILED (exit code $?)" | tee -a "$LOG_FILE"
        echo "" | tee -a "$LOG_FILE"
        ((FAILED++))
        return 1
    fi
}

# Example 1: shm_echo client (just prints info, no actual transport)
echo "=== Test 1: shm_echo client demo ===" | tee -a "$LOG_FILE"
run_test "shm_echo_client" go run ./shm_echo/client/main.go

# Example 2: shm_client_usage (demonstrates API usage)
echo "" | tee -a "$LOG_FILE"
echo "=== Test 2: shm_client_usage demo ===" | tee -a "$LOG_FILE"
run_test "shm_client_usage" go run ./shm_client_usage/main.go

# Example 3: shm_server_usage (demonstrates server API, times out after 10s)
echo "" | tee -a "$LOG_FILE"
echo "=== Test 3: shm_server_usage demo ===" | tee -a "$LOG_FILE"
run_test "shm_server_usage" timeout 15 go run ./shm_server_usage/main.go

# Example 4: helloworld_shm (full client-server test)
echo "" | tee -a "$LOG_FILE"
echo "=== Test 4: helloworld_shm client-server test ===" | tee -a "$LOG_FILE"

# Start server in background
echo "Starting helloworld_shm server..." | tee -a "$LOG_FILE"
go run ./helloworld_shm/greeter_server/main.go &
SERVER_PID=$!
sleep 2

# Run client
echo "Running helloworld_shm client..." | tee -a "$LOG_FILE"
if go run ./helloworld_shm/greeter_client/main.go -name="ShmTest" 2>&1 | tee -a "$LOG_FILE"; then
    echo "" | tee -a "$LOG_FILE"
    echo "✅ helloworld_shm: SUCCESS" | tee -a "$LOG_FILE"
    ((PASSED++))
else
    echo "" | tee -a "$LOG_FILE"
    echo "❌ helloworld_shm: FAILED" | tee -a "$LOG_FILE"
    ((FAILED++))
fi

# Stop server
kill $SERVER_PID 2>/dev/null
wait $SERVER_PID 2>/dev/null

# Clean up shared memory
rm -f /dev/shm/helloworld_shm 2>/dev/null

# Example 5: route_guide_shm (full client-server test with all RPC types)
echo "" | tee -a "$LOG_FILE"
echo "=== Test 5: route_guide_shm client-server test ===" | tee -a "$LOG_FILE"

# Start server in background
echo "Starting route_guide_shm server..." | tee -a "$LOG_FILE"
go run ./route_guide_shm/server/server.go &
SERVER_PID=$!
sleep 2

# Run client
echo "Running route_guide_shm client..." | tee -a "$LOG_FILE"
if timeout 30 go run ./route_guide_shm/client/client.go 2>&1 | tee -a "$LOG_FILE"; then
    echo "" | tee -a "$LOG_FILE"
    echo "✅ route_guide_shm: SUCCESS" | tee -a "$LOG_FILE"
    ((PASSED++))
else
    echo "" | tee -a "$LOG_FILE"
    echo "❌ route_guide_shm: FAILED" | tee -a "$LOG_FILE"
    ((FAILED++))
fi

# Stop server
kill $SERVER_PID 2>/dev/null
wait $SERVER_PID 2>/dev/null

# Clean up shared memory
rm -f /dev/shm/routeguide_shm 2>/dev/null

# Summary
echo "" | tee -a "$LOG_FILE"
echo "=========================================" | tee -a "$LOG_FILE"
echo "SUMMARY" | tee -a "$LOG_FILE"
echo "=========================================" | tee -a "$LOG_FILE"
echo "Passed: $PASSED" | tee -a "$LOG_FILE"
echo "Failed: $FAILED" | tee -a "$LOG_FILE"
echo "" | tee -a "$LOG_FILE"

if [ $FAILED -eq 0 ]; then
    echo "🎉 All examples ran successfully!" | tee -a "$LOG_FILE"
    exit 0
else
    echo "⚠️  Some examples failed. Check the log for details." | tee -a "$LOG_FILE"
    exit 1
fi
