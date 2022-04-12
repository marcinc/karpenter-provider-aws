export K8S_VERSION ?= 1.22.x
export KUBEBUILDER_ASSETS ?= ${HOME}/.kubebuilder/bin

## Inject the app version into project.Version
LDFLAGS ?= -ldflags=-X=github.com/aws/karpenter/pkg/utils/project.Version=$(shell git describe --tags --always)
GOFLAGS ?= -tags=$(CLOUD_PROVIDER) $(LDFLAGS)
WITH_GOFLAGS = GOFLAGS="$(GOFLAGS)"

## Extra helm options
CLUSTER_NAME ?= $(shell kubectl config view --minify -o jsonpath='{.clusters[].name}' | rev | cut -d"/" -f1 | rev)
CLUSTER_ENDPOINT ?= $(shell kubectl config view --minify -o jsonpath='{.clusters[].cluster.server}')
AWS_ACCOUNT_ID ?= $(shell aws sts get-caller-identity --output text | cut -d" " -f1)
KARPENTER_IAM_ROLE_ARN ?= arn:aws:iam::${AWS_ACCOUNT_ID}:role/${CLUSTER_NAME}-karpenter
HELM_OPTS ?= --set serviceAccount.annotations.eks\\.amazonaws\\.com/role-arn=${KARPENTER_IAM_ROLE_ARN} \
      		--set clusterName=${CLUSTER_NAME} \
			--set clusterEndpoint=${CLUSTER_ENDPOINT} \
			--set aws.defaultInstanceProfile=KarpenterNodeInstanceProfile-${CLUSTER_NAME}

help: ## Display help
	@awk 'BEGIN {FS = ":.*##"; printf "Usage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

dev: verify test ## Run all steps in the developer loop

ci: toolchain verify licenses battletest ## Run all steps used by continuous integration

test: ## Run tests
	ginkgo -r

strongertests:
	# Run randomized, racing, code coveraged, tests
	ginkgo -r \
			-cover -coverprofile=coverage.out -outputdir=. -coverpkg=./pkg/... \
			--randomizeAllSpecs --randomizeSuites -race

benchmark:
	go test -tags=test_performance -run=NoTests -bench=. ./...

deflake:
	for i in {1..10}; do make strongertests || exit 1; done
	ginkgo -r -race -tags random_test_delay

battletest: strongertests 
	go tool cover -html coverage.out -o coverage.html

verify: codegen ## Verify code. Includes dependencies, linting, formatting, etc
	go mod tidy
	go mod download
	golangci-lint run
	@git diff --quiet ||\
		{ echo "New file modification detected in the Git working tree. Please check in before commit.";\
		if [ $(MAKECMDGOALS) = 'ci' ]; then\
			exit 1;\
		fi;}

licenses: ## Verifies dependency licenses and requires GITHUB_TOKEN to be set
	$(WITH_GOFLAGS) go build -o karpenter cmd/controller/main.go
	golicense hack/license-config.hcl karpenter

apply: ## Deploy the controller from the current state of your git repository into your ~/.kube/config cluster
	helm upgrade --install karpenter charts/karpenter --namespace karpenter \
		$(HELM_OPTS) \
		--set controller.image=$(shell $(WITH_GOFLAGS) ko build -B github.com/aws/karpenter/cmd/controller) \
		--set webhook.image=$(shell $(WITH_GOFLAGS) ko build -B github.com/aws/karpenter/cmd/webhook)

install:  ## Deploy the latest released version into your ~/.kube/config cluster
	@echo Upgrading to $(shell grep version charts/karpenter/Chart.yaml)
	helm upgrade --install karpenter charts/karpenter --namespace karpenter \
		$(HELM_OPTS) 

delete: ## Delete the controller from your ~/.kube/config cluster
	helm uninstall karpenter --namespace karpenter

codegen: ## Generate code. Must be run if changes are made to ./pkg/apis/...
	controller-gen \
		object:headerFile="hack/boilerplate.go.txt" \
		crd \
		paths="./pkg/..." \
		output:crd:artifacts:config=charts/karpenter/crds
	hack/boilerplate.sh

release: ## Generate release manifests and publish a versioned container image.
	$(WITH_GOFLAGS) ./hack/release.sh

nightly: toolchain## Generate nightly release manifests and publish a versioned container image.
	$(WITH_GOFLAGS) ./hack/nightly.sh

snapshot: ## Generate nightly release manifests and publish a versioned container image.
	$(WITH_GOFLAGS) ./hack/snapshot.sh

toolchain: ## Install developer toolchain
	./hack/toolchain.sh

issues: ## Run GitHub issue analysis scripts
	pip install -r ./hack/github/requirements.txt
	@echo "Set GH_TOKEN env variable to avoid being rate limited by Github"
	./hack/github/feature_request_reactions.py > "karpenter-feature-requests-$(shell date +"%Y-%m-%d").csv"
	./hack/github/label_issue_count.py > "karpenter-labels-$(shell date +"%Y-%m-%d").csv"

website: ## Serve the docs website locally
	cd website && npm install && git submodule update --init --recursive && hugo server

.PHONY: help dev ci release test battletest verify codegen apply delete toolchain release licenses issues website
