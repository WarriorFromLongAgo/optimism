#!/usr/bin/env bash
set -euo pipefail

# Grab the script directory
SCRIPT_DIR=$(dirname "$0")

# Load common.sh
source "$SCRIPT_DIR/common.sh"

# Check if both input files are provided
if [ "$#" -ne 2 ]; then
    echo "Usage: $0 <deploy_config_path> <deployments_json_path>"
    exit 1
fi

# Set variables from environment or generate them
export NETWORK=${NETWORK:?NETWORK must be set}
export ETHERSCAN_API_KEY=${ETHERSCAN_API_KEY:?ETHERSCAN_API_KEY must be set}
export ETH_RPC_URL=${ETH_RPC_URL:?ETH_RPC_URL must be set}
export PRIVATE_KEY=${PRIVATE_KEY:?PRIVATE_KEY must be set}
export SYSTEM_OWNER_SAFE=${SYSTEM_OWNER_SAFE:?SYSTEM_OWNER_SAFE must be set}
export IMPL_SALT=${IMPL_SALT:?IMPL_SALT must be set}

# Check that network is either "mainnet" or "sepolia"
if [ "$NETWORK" != "mainnet" ] && [ "$NETWORK" != "sepolia" ]; then
  echo "Error: NETWORK must be either 'mainnet' or 'sepolia'"
  exit 1
fi

# Find the contracts-bedrock directory
CONTRACTS_BEDROCK_DIR=$(pwd)
while [[ "$CONTRACTS_BEDROCK_DIR" != "/" && "${CONTRACTS_BEDROCK_DIR##*/}" != "contracts-bedrock" ]]; do
    CONTRACTS_BEDROCK_DIR=$(dirname "$CONTRACTS_BEDROCK_DIR")
done

# Error out if we couldn't find it for some reason
if [[ "$CONTRACTS_BEDROCK_DIR" == "/" ]]; then
    echo "Error: 'contracts-bedrock' directory not found"
    exit 1
fi

# Set file paths from command-line arguments
export DEPLOY_CONFIG_PATH="$CONTRACTS_BEDROCK_DIR/deploy-config/deploy-config.json"
export DEPLOYMENTS_JSON_PATH="$CONTRACTS_BEDROCK_DIR/deployments/deployments.json"

# Copy the files into the paths so that the script can actually access it
cp $1 $DEPLOY_CONFIG_PATH
cp $2 $DEPLOYMENTS_JSON_PATH

# Set the StorageSetter address
export STORAGE_SETTER=0xd81f43edbcacb4c29a9ba38a13ee5d79278270cc

# Run deploy.sh
if ! "$SCRIPT_DIR/deploy.sh" | tee deploy.log; then
    echo "Error: deploy.sh failed"
    exit 1
fi

# Extract the address of the DisputeGameFactoryProxy from the deploy.log
export DISPUTE_GAME_FACTORY_PROXY=$(grep "0. DisputeGameFactoryProxy:" deploy.log | awk '{print $3}')
export ANCHOR_STATE_REGISTRY_PROXY=$(grep "1. AnchorStateRegistryProxy:" deploy.log | awk '{print $3}')
export ANCHOR_STATE_REGISTRY_IMPL=$(grep "2. AnchorStateRegistryImpl:" deploy.log | awk '{print $3}')
export PERMISSIONED_DELAYED_WETH_PROXY=$(grep "3. PermissionedDelayedWETHProxy:" deploy.log | awk '{print $3}')
export PERMISSIONED_DISPUTE_GAME=$(grep "4. PermissionedDisputeGame:" deploy.log | awk '{print $3}')

# Make sure everything was extracted properly
reqenv "DISPUTE_GAME_FACTORY_PROXY"
reqenv "ANCHOR_STATE_REGISTRY_PROXY"
reqenv "ANCHOR_STATE_REGISTRY_IMPL"
reqenv "PERMISSIONED_DELAYED_WETH_PROXY"
reqenv "PERMISSIONED_DISPUTE_GAME"

# Generate deployments.json with extracted addresses
cat << EOF > "deployments.json"
{
  "DisputeGameFactoryProxy": "$DISPUTE_GAME_FACTORY_PROXY",
  "AnchorStateRegistryProxy": "$ANCHOR_STATE_REGISTRY_PROXY",
  "AnchorStateRegistryImpl": "$ANCHOR_STATE_REGISTRY_IMPL",
  "PermissionedDelayedWETHProxy": "$PERMISSIONED_DELAYED_WETH_PROXY",
  "PermissionedDisputeGame": "$PERMISSIONED_DISPUTE_GAME"
}
EOF

# Run bundle.sh
if ! "$SCRIPT_DIR/bundle.sh" > bundle.json; then
    echo "Error: bundle.sh failed"
    exit 1
fi

# Run verify.sh
if ! "$SCRIPT_DIR/verify.sh" > validation.txt; then
    echo "Error: verify.sh failed"
    exit 1
fi

# Copy results into output directory
cp deploy.log /outputs/deploy.log
cp bundle.json /outputs/bundle.json
cp validation.txt /outputs/validation.txt
cp tmp-deployments.json /outputs/deployments.json
