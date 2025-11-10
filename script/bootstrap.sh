swag init --pd -d . -g ./cmd/dashboard/main.go -o ./cmd/dashboard/docs --requiredByDefault
protoc --go-grpc_out="require_unimplemented_servers=false:." --go_out="." proto/*.proto
rm -rf ../agent/proto
cp -r proto ../agent