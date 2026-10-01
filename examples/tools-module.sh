mkdir -p tools/botbox
cd tools/botbox
go mod init example.com/operator/tools/botbox
go mod edit -go=1.26.0
go get -tool github.com/rosenhouse/botbox/cmd/botbox@latest
cd ../..
go -C tools/botbox build -o ../../bin/botbox github.com/rosenhouse/botbox/cmd/botbox
bin/botbox version
