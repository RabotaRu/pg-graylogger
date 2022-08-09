all: local

local:
	GOOS=linux GOARCH=amd64 go build .

docker:
	docker build -t pg-graylogger --target=release .

