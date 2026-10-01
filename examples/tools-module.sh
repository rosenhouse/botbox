mkdir tools
cd tools
go mod init example.com/operator/tools
go mod edit -go=1.26.0
go get -tool github.com/rosenhouse/botbox/cmd/botbox@latest
cd ..
go -C tools build -o ../bin/botbox github.com/rosenhouse/botbox/cmd/botbox
bin/botbox version
