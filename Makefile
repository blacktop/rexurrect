.PHONY: check

check:
	test -z "$$(gofmt -l cmd internal)"
	go vet ./...
	go test -p 1 ./...
	go test ./internal/... -run TestIsolatedRoundTrip -v
	go test ./internal/... -run TestSaveStatusPrivateServer -v
	go test ./internal/... -run TestRestoreRecoveryPrivateServer -v
	go test ./internal/... -run TestAutoCyclePrivateServer -v
	go test ./cmd/... -run TestAutoBinaryPrivateServer -v
	go test ./internal/... -run TestLaunchAgentLifecycle -v
	go test ./internal/rex -run TestDefaultTerminalRoundTrip -v
	go test ./internal/rex -run TestAgentResumeRoundTrip -v
