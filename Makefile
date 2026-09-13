NAMESPACE ?= migration-test
PROXY_IMAGE ?= apimigrate/proxy:latest
MOCK_IMAGE ?= apimigrate/mock:latest

.PHONY: help build-proxy build-mock images deploy undeploy report report-json status logs example-local clean

help: ## list targets
	@grep -E '^[a-z_-]+:.*?## ' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'

build-proxy: ## build the migration proxy image (uses local Docker daemon)
	docker build --build-arg PKG=. -t $(PROXY_IMAGE) .

build-mock: ## build the example mock service image
	docker build --build-arg PKG=./example/mockservice -t $(MOCK_IMAGE) .

images: build-proxy build-mock ## build both images

deploy: images ## build images and deploy everything to the current kube context
	kubectl apply -f example/k8s/namespace.yaml   # namespace + configmap
	kubectl apply -f example/k8s/mock.yaml
	kubectl apply -f example/k8s/proxy.yaml
	kubectl apply -f example/k8s/traffic.yaml
	kubectl -n $(NAMESPACE) rollout status deploy/mock-original --timeout=120s
	kubectl -n $(NAMESPACE) rollout status deploy/mock-migrated --timeout=120s
	kubectl -n $(NAMESPACE) rollout status deploy/migration-proxy --timeout=120s

undeploy: ## remove namespace and all example resources
	kubectl delete namespace $(NAMESPACE) --ignore-not-found

example-local: ## run the local (no-k8s) end-to-end demo then clean itself up
	./example/demo-local.sh

status: ## show deployment status
	kubectl -n $(NAMESPACE) get deploy,pods,svc -o wide

report: ## fetch the markdown report via a one-shot port-forward
	@kubectl -n $(NAMESPACE) port-forward svc/migration-proxy 18080:8080 >/dev/null 2>&1 & pid=$$!; \
	trap 'kill $$pid 2>/dev/null' EXIT; \
	sleep 2; \
	curl -s http://127.0.0.1:18080/report; echo

report-json: ## fetch the JSON report
	@kubectl -n $(NAMESPACE) port-forward svc/migration-proxy 18080:8080 >/dev/null 2>&1 & pid=$$!; \
	trap 'kill $$pid 2>/dev/null' EXIT; \
	sleep 2; \
	curl -s http://127.0.0.1:18080/report.json | head -80; echo

logs: ## tail proxy + traffic logs
	kubectl -n $(NAMESPACE) logs deploy/migration-proxy --tail=20
	kubectl -n $(NAMESPACE) logs deploy/traffic-generator --tail=5

clean: undeploy ## aliased cleanup