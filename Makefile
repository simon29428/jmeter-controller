IMAGE ?= jmeter-controller:latest

.PHONY: build
build:
	go build -o bin/controller.exe ./cmd/main.go

.PHONY: test
test:
	go test ./... -v

.PHONY: fmt
fmt:
	go fmt ./...

.PHONY: vet
vet:
	go vet ./...

.PHONY: docker-build
docker-build:
	docker build -t $(IMAGE) .

.PHONY: docker-push
docker-push:
	docker push $(IMAGE)

.PHONY: install
install:
	kubectl apply -f config/crd/

.PHONY: uninstall
uninstall:
	kubectl delete -f config/crd/ --ignore-not-found

.PHONY: deploy
deploy:
	kubectl apply -f config/crd/
	kubectl apply -f config/rbac/
	kubectl apply -f config/manager/

.PHONY: undeploy
undeploy:
	kubectl delete -f config/manager/ --ignore-not-found
	kubectl delete -f config/rbac/ --ignore-not-found
	kubectl delete -f config/crd/ --ignore-not-found

.PHONY: run
run:
	go run ./cmd/main.go --config config/samples/controller_config.yaml
