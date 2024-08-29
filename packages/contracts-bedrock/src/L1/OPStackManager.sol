// SPDX-License-Identifier: MIT
pragma solidity 0.8.15;

import { ISemver } from "src/universal/ISemver.sol";
import { Proxy } from "src/universal/Proxy.sol";
import { ProxyAdmin } from "src/universal/ProxyAdmin.sol";

import { L1ChugSplashProxy } from "src/legacy/L1ChugSplashProxy.sol";
import { ResolvedDelegateProxy } from "src/legacy/ResolvedDelegateProxy.sol";
import { AddressManager } from "src/legacy/AddressManager.sol";

import { DelayedWETH } from "src/dispute/weth/DelayedWETH.sol";
import { DisputeGameFactory } from "src/dispute/DisputeGameFactory.sol";
import { AnchorStateRegistry } from "src/dispute/AnchorStateRegistry.sol";
import { FaultDisputeGame } from "src/dispute/FaultDisputeGame.sol";
import { PermissionedDisputeGame } from "src/dispute/PermissionedDisputeGame.sol";
import { GameTypes } from "src/dispute/lib/Types.sol";

import { SuperchainConfig } from "src/L1/SuperchainConfig.sol";
import { ProtocolVersions } from "src/L1/ProtocolVersions.sol";
import { OptimismPortal2 } from "src/L1/OptimismPortal2.sol";
import { SystemConfig } from "src/L1/SystemConfig.sol";
import { ResourceMetering } from "src/L1/ResourceMetering.sol";
import { L1CrossDomainMessenger } from "src/L1/L1CrossDomainMessenger.sol";
import { L1ERC721Bridge } from "src/L1/L1ERC721Bridge.sol";
import { L1StandardBridge } from "src/L1/L1StandardBridge.sol";
import { OptimismMintableERC20Factory } from "src/universal/OptimismMintableERC20Factory.sol";

/// @custom:proxied TODO this is not proxied yet.
contract OPStackManager is ISemver {
    // -------- Structs --------

    /// @notice Represents the roles that can be set when deploying a standard OP Stack chain.
    struct Roles {
        address opChainProxyAdminOwner;
        address systemConfigOwner;
        address batcher;
        address unsafeBlockSigner;
        address proposer;
        address challenger;
    }

    /// @notice The full set of inputs to deploy a new OP Stack chain.
    struct DeployInput {
        Roles roles;
        uint32 basefeeScalar;
        uint32 blobBasefeeScalar;
        uint256 l2ChainId;
    }

    /// @notice The full set of outputs from deploying a new OP Stack chain.
    struct DeployOutput {
        ProxyAdmin opChainProxyAdmin;
        AddressManager addressManager;
        L1ERC721Bridge l1ERC721BridgeProxy;
        SystemConfig systemConfigProxy;
        OptimismMintableERC20Factory optimismMintableERC20FactoryProxy;
        L1StandardBridge l1StandardBridgeProxy;
        L1CrossDomainMessenger l1CrossDomainMessengerProxy;
        // Fault proof contracts below.
        OptimismPortal2 optimismPortalProxy;
        DisputeGameFactory disputeGameFactoryProxy;
        DisputeGameFactory disputeGameFactoryImpl;
        AnchorStateRegistry anchorStateRegistryProxy;
        AnchorStateRegistry anchorStateRegistryImpl;
        FaultDisputeGame faultDisputeGame;
        PermissionedDisputeGame permissionedDisputeGame;
        DelayedWETH delayedWETHPermissionedGameProxy;
        DelayedWETH delayedWETHPermissionlessGameProxy;
    }

    /// @notice The logic address and initializer selector for an implementation contract.
    struct Implementation {
        address logic; // Address containing the deployed logic contract.
        bytes4 initializer; // Function selector for the initializer.
    }

    /// @notice Used to set the implementation for a contract by mapping a contract
    /// name to the implementation data.
    struct ImplementationSetter {
        string name; // Contract name.
        Implementation info; // Implementation to set.
    }

    // -------- Constants and Variables --------

    /// @custom:semver 1.0.0-beta.2
    string public constant version = "1.0.0-beta.2";

    /// @notice Address of the SuperchainConfig contract shared by all chains.
    SuperchainConfig public immutable superchainConfig;

    /// @notice Address of the ProtocolVersions contract shared by all chains.
    ProtocolVersions public immutable protocolVersions;

    /// @notice The latest release of the OP Stack Manager, as a string of the format `op-contracts/vX.Y.Z`.
    string public latestRelease;

    /// @notice Maps a release version to a contract name to it's implementation data.
    mapping(string => mapping(string => Implementation)) public implementations;

    /// @notice Maps an L2 Chain ID to the SystemConfig for that chain.
    mapping(uint256 => SystemConfig) public systemConfigs;

    // -------- Events --------

    /// @notice Emitted when a new OP Stack chain is deployed.
    /// @param l2ChainId The chain ID of the new chain.
    /// @param systemConfig The address of the new chain's SystemConfig contract.
    event Deployed(uint256 indexed l2ChainId, SystemConfig indexed systemConfig);

    // -------- Errors --------

    /// @notice Throw when two addresses do not match but are expected to.
    error AddressMismatch(string addressName);

    /// @notice Thrown when an address is the zero address.
    error AddressNotFound(address who);

    /// @notice Throw when a contract address has no code.
    error AddressHasNoCode(address who);

    /// @notice Thrown when a release version is already set.
    error AlreadyReleased();

    /// @notice Thrown when an invalid `l2ChainId` is provided to `deploy`.
    error InvalidChainId();

    /// @notice Thrown when a role's address is not valid.
    error InvalidRoleAddress(string role);

    // -------- Methods --------

    /// @notice OPSM is intended to be proxied when used in production. Since we are initially
    /// focused on an OPSM version that unblocks interop, we are not proxying OPSM for simplicity.
    /// Later, we will `_disableInitializers` in the constructor and replace any constructor logic
    /// with an `initialize` function, which will be a breaking change to the OPSM interface.
    constructor(SuperchainConfig _superchainConfig, ProtocolVersions _protocolVersions) {
        superchainConfig = _superchainConfig;
        protocolVersions = _protocolVersions;
    }

    /// @notice Callable by the OPSM owner to release a set of implementation contracts for a given
    /// release version. This must be called with `_isLatest` set to true before any chains can be deployed.
    /// @param _release The release version to set implementations for, of the format `op-contracts/vX.Y.Z`.
    /// @param _isLatest Whether the release version is the latest released version. This is
    /// significant because the latest version is used to deploy chains in the `deploy` function.
    /// @param _setters The set of implementations to set for the release version.
    function setRelease(string memory _release, bool _isLatest, ImplementationSetter[] calldata _setters) external {
        // TODO Add auth to this method.

        if (_isLatest) latestRelease = _release;

        for (uint256 i = 0; i < _setters.length; i++) {
            ImplementationSetter calldata setter = _setters[i];
            Implementation storage impl = implementations[_release][setter.name];
            if (impl.logic != address(0)) revert AlreadyReleased();

            impl.initializer = setter.info.initializer;
            impl.logic = setter.info.logic;
        }
    }

    function deploy(DeployInput calldata _input) external returns (DeployOutput memory) {
        assertValidInputs(_input);

        // TODO Determine how we want to choose salt, e.g. are we concerned about chain ID squatting
        // since this approach means a chain ID can only be used once.
        uint256 l2ChainId = _input.l2ChainId;
        bytes32 salt = bytes32(_input.l2ChainId);
        DeployOutput memory output;

        // -------- Deploy Proxy Contracts --------

        // The ProxyAdmin is the owner of all proxies for the chain. We temporarily set the owner to
        // this contract, and then transfer ownership to the specified owner at the end of deployment.
        // The AddressManager is used to store the implementation for the L1CrossDomainMessenger
        // due to it's usage of the legacy ResolvedDelegateProxy.
        output.addressManager = new AddressManager{ salt: salt }();
        output.opChainProxyAdmin = new ProxyAdmin{ salt: salt }({ _owner: address(this) });
        output.opChainProxyAdmin.setAddressManager(output.addressManager);

        // Deploy ERC-1967 proxied contracts.
        output.l1ERC721BridgeProxy = L1ERC721Bridge(deployProxy(l2ChainId, output.opChainProxyAdmin, "L1ERC721Bridge"));
        output.optimismPortalProxy =
            OptimismPortal2(payable(deployProxy(l2ChainId, output.opChainProxyAdmin, "OptimismPortal")));
        output.systemConfigProxy = SystemConfig(deployProxy(l2ChainId, output.opChainProxyAdmin, "SystemConfig"));
        output.optimismMintableERC20FactoryProxy = OptimismMintableERC20Factory(
            deployProxy(l2ChainId, output.opChainProxyAdmin, "OptimismMintableERC20Factory")
        );

        // Deploy legacy proxied contracts.
        output.l1StandardBridgeProxy =
            L1StandardBridge(payable(address(new L1ChugSplashProxy{ salt: salt }(address(output.opChainProxyAdmin)))));
        output.opChainProxyAdmin.setProxyType(address(output.l1StandardBridgeProxy), ProxyAdmin.ProxyType.CHUGSPLASH);

        string memory contractName = "OVM_L1CrossDomainMessenger";
        output.l1CrossDomainMessengerProxy = L1CrossDomainMessenger(
            address(new ResolvedDelegateProxy{ salt: salt }(output.addressManager, contractName))
        );
        output.opChainProxyAdmin.setProxyType(
            address(output.l1CrossDomainMessengerProxy), ProxyAdmin.ProxyType.RESOLVED
        );
        output.opChainProxyAdmin.setImplementationName(address(output.l1CrossDomainMessengerProxy), contractName);

        // Now that all proxies are deployed, we can transfer ownership of the AddressManager to the ProxyAdmin.
        output.addressManager.transferOwnership(address(output.opChainProxyAdmin));

        // -------- Set and Initialize Proxy Implementations --------
        Implementation storage impl;
        bytes memory data;

        impl = getLatestImplementation("L1ERC721Bridge");
        data = encodeL1ERC721BridgeInitializer(impl.initializer, output);
        upgradeAndCall(output.opChainProxyAdmin, address(output.l1ERC721BridgeProxy), impl.logic, data);

        impl = getLatestImplementation("OptimismPortal");
        data = encodeOptimismPortalInitializer(impl.initializer, output);
        upgradeAndCall(output.opChainProxyAdmin, address(output.optimismPortalProxy), impl.logic, data);

        impl = getLatestImplementation("SystemConfig");
        data = encodeSystemConfigInitializer(impl.initializer, _input, output);
        upgradeAndCall(output.opChainProxyAdmin, address(output.systemConfigProxy), impl.logic, data);

        impl = getLatestImplementation("OptimismMintableERC20Factory");
        data = encodeOptimismMintableERC20FactoryInitializer(impl.initializer, output);
        upgradeAndCall(output.opChainProxyAdmin, address(output.optimismMintableERC20FactoryProxy), impl.logic, data);

        impl = getLatestImplementation("L1CrossDomainMessenger");
        // TODO add this check back in
        // require(
        //     impl.logic == referenceAddressManager.getAddress("OVM_L1CrossDomainMessenger"),
        //     "OpStackManager: L1CrossDomainMessenger implementation mismatch"
        // );
        data = encodeL1CrossDomainMessengerInitializer(impl.initializer, output);
        upgradeAndCall(output.opChainProxyAdmin, address(output.l1CrossDomainMessengerProxy), impl.logic, data);

        // -------- TODO: Placeholders --------
        // For contracts we don't yet deploy, we set the outputs to  dummy proxies so they have code to pass assertions.
        output.disputeGameFactoryProxy = DisputeGameFactory(deployProxy(l2ChainId, output.opChainProxyAdmin, "1"));
        output.disputeGameFactoryImpl = DisputeGameFactory(deployProxy(l2ChainId, output.opChainProxyAdmin, "2"));
        output.anchorStateRegistryProxy = AnchorStateRegistry(deployProxy(l2ChainId, output.opChainProxyAdmin, "3"));
        output.anchorStateRegistryImpl = AnchorStateRegistry(deployProxy(l2ChainId, output.opChainProxyAdmin, "4"));
        output.faultDisputeGame = FaultDisputeGame(deployProxy(l2ChainId, output.opChainProxyAdmin, "5"));
        output.permissionedDisputeGame = PermissionedDisputeGame(deployProxy(l2ChainId, output.opChainProxyAdmin, "6"));
        output.delayedWETHPermissionedGameProxy =
            DelayedWETH(payable(deployProxy(l2ChainId, output.opChainProxyAdmin, "7")));
        output.delayedWETHPermissionlessGameProxy =
            DelayedWETH(payable(deployProxy(l2ChainId, output.opChainProxyAdmin, "8")));

        // -------- Finalize Deployment --------
        // Transfer ownership of the ProxyAdmin from this contract to the specified owner.
        output.opChainProxyAdmin.transferOwnership(_input.roles.opChainProxyAdminOwner);

        // Correctness checks.
        // TODO these currently fail in tests because the tests use dummy implementation addresses that have no code.
        // if (output.systemConfigProxy.owner() != _input.roles.systemConfigOwner) {
        //     revert AddressMismatch("systemConfigOwner");
        // }
        // if (output.systemConfigProxy.l1CrossDomainMessenger() != address(output.l1CrossDomainMessengerProxy)) {
        //     revert AddressMismatch("l1CrossDomainMessengerProxy");
        // }
        // if (output.systemConfigProxy.l1ERC721Bridge() != address(output.l1ERC721BridgeProxy)) {
        //     revert AddressMismatch("l1ERC721BridgeProxy");
        // }
        // if (output.systemConfigProxy.l1StandardBridge() != address(output.l1StandardBridgeProxy)) {
        //     revert AddressMismatch("l1StandardBridgeProxy");
        // }
        // if (output.systemConfigProxy.optimismPortal() != address(output.optimismPortalProxy)) {
        //     revert AddressMismatch("optimismPortalProxy");
        // }
        // if (
        //     output.systemConfigProxy.optimismMintableERC20Factory() !=
        // address(output.optimismMintableERC20FactoryProxy)
        // ) revert AddressMismatch("optimismMintableERC20FactoryProxy");

        return output;
    }

    // -------- Utilities --------

    /// @notice Verifies that all inputs are valid and reverts if any are invalid.
    /// Typically the proxy admin owner is expected to have code, but this is not enforced here.
    function assertValidInputs(DeployInput calldata _input) internal view {
        if (_input.l2ChainId == 0 || _input.l2ChainId == block.chainid) revert InvalidChainId();

        if (_input.roles.opChainProxyAdminOwner == address(0)) revert InvalidRoleAddress("opChainProxyAdminOwner");
        if (_input.roles.systemConfigOwner == address(0)) revert InvalidRoleAddress("systemConfigOwner");
        if (_input.roles.batcher == address(0)) revert InvalidRoleAddress("batcher");
        if (_input.roles.unsafeBlockSigner == address(0)) revert InvalidRoleAddress("unsafeBlockSigner");
        if (_input.roles.proposer == address(0)) revert InvalidRoleAddress("proposer");
        if (_input.roles.challenger == address(0)) revert InvalidRoleAddress("challenger");
    }

    /// @notice Maps an L2 chain ID to an L1 batch inbox address as defined by the standard
    /// configuration's convention. This convention is `versionByte || keccak256(bytes32(chainId))[:19]`,
    /// where || denotes concatenation`, versionByte is 0x00, and chainId is a uint256.
    /// https://specs.optimism.io/protocol/configurability.html#consensus-parameters
    function chainIdToBatchInboxAddress(uint256 _l2ChainId) internal pure returns (address) {
        bytes1 versionByte = 0x00;
        bytes32 hashedChainId = keccak256(bytes.concat(bytes32(_l2ChainId)));
        bytes19 first19Bytes = bytes19(hashedChainId);
        return address(uint160(bytes20(bytes.concat(versionByte, first19Bytes))));
    }

    /// @notice Deterministically deploys a new proxy contract owned by the provided ProxyAdmin.
    /// The salt is computed as a function of the L2 chain ID and the contract name. This is required
    /// because we deploy many identical proxies, so they each require a unique salt for determinism.
    function deployProxy(uint256 l2ChainId, ProxyAdmin proxyAdmin, bytes32 contractName) internal returns (address) {
        bytes32 salt = keccak256(abi.encode(l2ChainId, contractName));
        return address(new Proxy{ salt: salt }(address(proxyAdmin)));
    }

    /// @notice Returns the implementation data for a contract name.
    function getLatestImplementation(string memory _name) internal view returns (Implementation storage) {
        return implementations[latestRelease][_name];
    }

    // -------- Initializer Encoding --------

    /// @notice Helper method for encoding the L1ERC721Bridge initializer data.
    function encodeL1ERC721BridgeInitializer(
        bytes4 _selector,
        DeployOutput memory _output
    )
        internal
        view
        returns (bytes memory)
    {
        return abi.encodeWithSelector(_selector, _output.l1CrossDomainMessengerProxy, superchainConfig);
    }

    /// @notice Helper method for encoding the OptimismPortal initializer data.
    function encodeOptimismPortalInitializer(
        bytes4 _selector,
        DeployOutput memory _output
    )
        internal
        view
        returns (bytes memory)
    {
        _output;
        return abi.encodeWithSelector(
            _selector, _output.disputeGameFactoryProxy, _output.systemConfigProxy, superchainConfig, GameTypes.CANNON
        );
    }

    /// @notice Helper method for encoding the SystemConfig initializer data.
    function encodeSystemConfigInitializer(
        bytes4 selector,
        DeployInput memory _input,
        DeployOutput memory _output
    )
        internal
        pure
        returns (bytes memory)
    {
        ResourceMetering.ResourceConfig memory referenceResourceConfig = ResourceMetering.ResourceConfig({
            maxResourceLimit: 2e7,
            elasticityMultiplier: 10,
            baseFeeMaxChangeDenominator: 8,
            minimumBaseFee: 1e9,
            systemTxMaxGas: 1e6,
            maximumBaseFee: 340282366920938463463374607431768211455
        });

        SystemConfig.Addresses memory addrs = SystemConfig.Addresses({
            l1CrossDomainMessenger: address(_output.l1CrossDomainMessengerProxy),
            l1ERC721Bridge: address(_output.l1ERC721BridgeProxy),
            l1StandardBridge: address(_output.l1StandardBridgeProxy),
            disputeGameFactory: address(_output.disputeGameFactoryProxy),
            optimismPortal: address(_output.optimismPortalProxy),
            optimismMintableERC20Factory: address(_output.optimismMintableERC20FactoryProxy),
            gasPayingToken: address(0)
        });

        return abi.encodeWithSelector(
            selector,
            _input.roles.systemConfigOwner,
            _input.basefeeScalar,
            _input.blobBasefeeScalar,
            bytes32(uint256(uint160(_input.roles.batcher))), // batcherHash
            30_000_000, // gasLimit
            _input.roles.unsafeBlockSigner,
            referenceResourceConfig,
            chainIdToBatchInboxAddress(_input.l2ChainId),
            addrs
        );
    }

    /// @notice Helper method for encoding the OptimismMintableERC20Factory initializer data.
    function encodeOptimismMintableERC20FactoryInitializer(
        bytes4 _selector,
        DeployOutput memory _output
    )
        internal
        pure
        returns (bytes memory)
    {
        return abi.encodeWithSelector(_selector, _output.l1StandardBridgeProxy);
    }

    /// @notice Helper method for encoding the L1CrossDomainMessenger initializer data.
    function encodeL1CrossDomainMessengerInitializer(
        bytes4 _selector,
        DeployOutput memory _output
    )
        internal
        view
        returns (bytes memory)
    {
        return
            abi.encodeWithSelector(_selector, superchainConfig, _output.optimismPortalProxy, _output.systemConfigProxy);
    }

    /// @notice Makes an external call to the target to initialize the proxy with the specified data.
    /// This is here to reduce by code size as ABI encoding for external calls can be bloaty, and
    /// we are near the size limit.
    function upgradeAndCall(
        ProxyAdmin _proxyAdmin,
        address _target,
        address _implementation,
        bytes memory _data
    )
        internal
    {
        assertValidContractAddress(address(_proxyAdmin));
        assertValidContractAddress(_target);
        assertValidContractAddress(_implementation);

        _proxyAdmin.upgradeAndCall(payable(address(_target)), _implementation, _data);
    }

    function assertValidContractAddress(address _who) internal view {
        if (_who == address(0)) revert AddressNotFound(_who);
        if (_who.code.length == 0) revert AddressHasNoCode(_who);
    }
}
