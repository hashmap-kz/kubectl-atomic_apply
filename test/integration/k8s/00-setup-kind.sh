#!/bin/bash
set -euo pipefail

echo "✓ Deleting kind cluster..."
kind delete cluster --name katomik || echo "Kind cluster not found"

echo "✓ Running kind cluster..."
kind create cluster --config=kind-config.yaml
kubectl config set-context "kind-katomik"
