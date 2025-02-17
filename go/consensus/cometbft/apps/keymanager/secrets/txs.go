package secrets

import (
	"fmt"
	"slices"

	"github.com/oasisprotocol/curve25519-voi/primitives/x25519"

	"github.com/oasisprotocol/oasis-core/go/common"
	"github.com/oasisprotocol/oasis-core/go/common/crypto/signature"
	"github.com/oasisprotocol/oasis-core/go/common/node"
	tmapi "github.com/oasisprotocol/oasis-core/go/consensus/cometbft/api"
	secretsState "github.com/oasisprotocol/oasis-core/go/consensus/cometbft/apps/keymanager/secrets/state"
	registryState "github.com/oasisprotocol/oasis-core/go/consensus/cometbft/apps/registry/state"
	"github.com/oasisprotocol/oasis-core/go/keymanager/api"
	"github.com/oasisprotocol/oasis-core/go/keymanager/secrets"
	registry "github.com/oasisprotocol/oasis-core/go/registry/api"
)

func (ext *secretsExt) updatePolicy(
	ctx *tmapi.Context,
	state *secretsState.MutableState,
	sigPol *secrets.SignedPolicySGX,
) error {
	// Ensure that the runtime exists and is a key manager.
	regState := registryState.NewMutableState(ctx.State())
	kmRt, err := keyManagerRuntime(ctx, regState, sigPol.Policy.ID)
	if err != nil {
		return err
	}

	// Ensure that the tx signer is the key manager owner.
	if !kmRt.EntityID.Equal(ctx.TxSigner()) {
		return fmt.Errorf("keymanager: invalid update signer: %s", sigPol.Policy.ID)
	}

	// Get the existing policy document, if one exists.
	oldStatus, err := state.Status(ctx, kmRt.ID)
	switch err {
	case nil:
	case secrets.ErrNoSuchStatus:
		// This must be a new key manager runtime.
		oldStatus = &secrets.Status{
			ID: kmRt.ID,
		}
	default:
		return err
	}

	// Validate the tx.
	if err = secrets.SanityCheckSignedPolicySGX(oldStatus.Policy, sigPol); err != nil {
		return err
	}

	if ctx.IsCheckOnly() {
		return nil
	}

	// Charge gas for this operation.
	kmParams, err := state.ConsensusParameters(ctx)
	if err != nil {
		return err
	}
	if err = ctx.Gas().UseGas(1, secrets.GasOpUpdatePolicy, kmParams.GasCosts); err != nil {
		return err
	}

	// Return early if simulating since this is just estimating gas.
	if ctx.IsSimulation() {
		return nil
	}

	// Ok, as far as we can tell the new policy is valid, apply it.
	//
	// Note: The key manager cohort responsible for servicing this ID
	// will be unresponsive for a minimum of one epoch as a new cohort
	// will only be formed on the epoch transition.  If replication
	// is in the picture, the replication process will take an
	// additional epoch.
	//
	// TODO: It would be possible to update the cohort on each
	// node-reregistration, but I'm not sure how often the policy
	// will get updated.
	epoch, err := ext.state.GetCurrentEpoch(ctx)
	if err != nil {
		return err
	}

	regParams, err := regState.ConsensusParameters(ctx)
	if err != nil {
		return err
	}

	nodes, _ := regState.Nodes(ctx)
	registry.SortNodeList(nodes)
	oldStatus.Policy = sigPol
	newStatus := generateStatus(ctx, kmRt, oldStatus, nil, nodes, regParams, epoch)
	if err := state.SetStatus(ctx, newStatus); err != nil {
		ctx.Logger().Error("keymanager: failed to set key manager status",
			"err", err,
		)
		return fmt.Errorf("keymanager: failed to set key manager status: %w", err)
	}

	ctx.EmitEvent(tmapi.NewEventBuilder(ext.appName).TypedAttribute(&secrets.StatusUpdateEvent{
		Statuses: []*secrets.Status{newStatus},
	}))

	return nil
}

// publishMasterSecret stores a new proposal for the master secret, which may overwrite
// the previous one.
//
// Key managers have the ability to rotate the master secret at predetermined intervals.
// Each rotation introduces a new generation, or version, of the master secret that is
// sequentially numbered, starting from zero. These rotations occur during key manager
// status updates, which typically happen during epoch transitions. To perform a rotation,
// one of the key manager enclaves must publish a proposal for the next generation of
// the master secret, which must then be replicated by the majority of enclaves.
// If the replication process is not completed by the end of the epoch, the proposal can
// be replaced with a new one.
//
// Since key managers have to store all generations of the master secret, rotations should
// not take place too frequently. The frequency of rotations does not affect runtimes,
// as they can skip generations when performing state re-encryptions.
//
// It's worth noting that the process of generating, publishing, and replicating master
// secrets differs from that of ephemeral secrets. For more information, please refer
// to the description of the publishEphemeralSecret function.
func (ext *secretsExt) publishMasterSecret(
	ctx *tmapi.Context,
	state *secretsState.MutableState,
	secret *secrets.SignedEncryptedMasterSecret,
) error {
	// Ensure that the runtime exists and is a key manager.
	regState := registryState.NewMutableState(ctx.State())
	kmRt, err := keyManagerRuntime(ctx, regState, secret.Secret.ID)
	if err != nil {
		return err
	}

	// Reject if the signer is not in the key manager committee.
	kmStatus, err := state.Status(ctx, kmRt.ID)
	if err != nil {
		return err
	}
	if !slices.Contains(kmStatus.Nodes, ctx.TxSigner()) {
		return fmt.Errorf("keymanager: master secret can be published only by the key manager committee")
	}

	// Reject if the master secret has been proposed in this epoch.
	lastSecret, err := state.MasterSecret(ctx, secret.Secret.ID)
	if err != nil && err != secrets.ErrNoSuchMasterSecret {
		return err
	}
	if lastSecret != nil && secret.Secret.Epoch == lastSecret.Secret.Epoch {
		return fmt.Errorf("keymanager: master secret can be proposed once per epoch")
	}

	// Reject if rotation is not allowed.
	if err = kmStatus.VerifyRotationEpoch(secret.Secret.Epoch); err != nil {
		return fmt.Errorf("keymanager: master secret rotation not allowed: %w", err)
	}

	// Verify the secret. Master secrets can be published for the next epoch and for
	// the next generation only.
	nextGen := kmStatus.NextGeneration()
	epoch, err := ext.state.GetCurrentEpoch(ctx)
	if err != nil {
		return err
	}
	nextEpoch := epoch + 1
	rak, err := runtimeAttestationKey(ctx, regState, kmRt)
	if err != nil {
		return err
	}
	reks := runtimeEncryptionKeys(ctx, regState, kmRt, kmStatus)

	if err = secret.Verify(nextGen, nextEpoch, reks, rak); err != nil {
		return err
	}

	// Return early if this is a CheckTx context.
	if ctx.IsCheckOnly() {
		return nil
	}

	// Charge gas for this operation.
	kmParams, err := state.ConsensusParameters(ctx)
	if err != nil {
		return err
	}
	if err = ctx.Gas().UseGas(1, secrets.GasOpPublishMasterSecret, kmParams.GasCosts); err != nil {
		return err
	}

	// Return early if simulating since this is just estimating gas.
	if ctx.IsSimulation() {
		return nil
	}

	// Ok, as far as we can tell the secret is valid, save it.
	if err := state.SetMasterSecret(ctx, secret); err != nil {
		ctx.Logger().Error("keymanager: failed to set key manager master secret",
			"err", err,
		)
		return fmt.Errorf("keymanager: failed to set key manager master secret: %w", err)
	}

	ctx.EmitEvent(tmapi.NewEventBuilder(ext.appName).TypedAttribute(&secrets.MasterSecretPublishedEvent{
		Secret: secret,
	}))

	return nil
}

// publishEphemeralSecret stores the ephemeral secret for the given epoch.
//
// Key managers support forward-secret ephemeral secrets which are never encrypted with SGX sealing
// key nor stored in the enclave's cold storage. These secrets are generated by the enclaves
// themselves for the next epoch only and published encrypted in the consensus layer.
// Only one secret can be published for an epoch, others are discarded. Overwrites are not
// allowed as with master secrets. So if all enclaves restart at the same time, no one
// will be able to decrypt ephemeral secrets for the past. The number of generated secrets
// does not affect the performance, as key managers store in memory only the last few secrets,
// as defined in the policy.
//
// Note that ephemeral secrets differ from master secrets. For more information, see
// the description of the publishMasterSecret function.
func (ext *secretsExt) publishEphemeralSecret(
	ctx *tmapi.Context,
	state *secretsState.MutableState,
	secret *secrets.SignedEncryptedEphemeralSecret,
) error {
	// Ensure that the runtime exists and is a key manager.
	regState := registryState.NewMutableState(ctx.State())
	kmRt, err := keyManagerRuntime(ctx, regState, secret.Secret.ID)
	if err != nil {
		return err
	}

	// Reject if the signer is not in the key manager committee.
	kmStatus, err := state.Status(ctx, kmRt.ID)
	if err != nil {
		return err
	}
	if !slices.Contains(kmStatus.Nodes, ctx.TxSigner()) {
		return fmt.Errorf("keymanager: ephemeral secret can be published only by the key manager committee")
	}

	// Reject if the ephemeral secret has been published in this epoch.
	lastSecret, err := state.EphemeralSecret(ctx, secret.Secret.ID)
	if err != nil && err != secrets.ErrNoSuchEphemeralSecret {
		return err
	}
	if lastSecret != nil && secret.Secret.Epoch == lastSecret.Secret.Epoch {
		return fmt.Errorf("keymanager: ephemeral secret can be proposed once per epoch")
	}

	// Verify the secret. Ephemeral secrets can be published for the next epoch only.
	epoch, err := ext.state.GetCurrentEpoch(ctx)
	if err != nil {
		return err
	}
	nextEpoch := epoch + 1
	rak, err := runtimeAttestationKey(ctx, regState, kmRt)
	if err != nil {
		return err
	}
	reks := runtimeEncryptionKeys(ctx, regState, kmRt, kmStatus)

	if err = secret.Verify(nextEpoch, reks, rak); err != nil {
		return err
	}

	// Return early if this is a CheckTx context.
	if ctx.IsCheckOnly() {
		return nil
	}

	// Charge gas for this operation.
	kmParams, err := state.ConsensusParameters(ctx)
	if err != nil {
		return err
	}
	if err = ctx.Gas().UseGas(1, secrets.GasOpPublishEphemeralSecret, kmParams.GasCosts); err != nil {
		return err
	}

	// Return early if simulating since this is just estimating gas.
	if ctx.IsSimulation() {
		return nil
	}

	// Ok, as far as we can tell the secret is valid, save it.
	if err := state.SetEphemeralSecret(ctx, secret); err != nil {
		ctx.Logger().Error("keymanager: failed to set key manager ephemeral secret",
			"err", err,
		)
		return fmt.Errorf("keymanager: failed to set key manager ephemeral secret: %w", err)
	}

	ctx.EmitEvent(tmapi.NewEventBuilder(ext.appName).TypedAttribute(&secrets.EphemeralSecretPublishedEvent{
		Secret: secret,
	}))

	return nil
}

func keyManagerRuntime(ctx *tmapi.Context, regState *registryState.MutableState, id common.Namespace) (*registry.Runtime, error) {
	// Ensure that the runtime exists and is a key manager.
	rt, err := regState.Runtime(ctx, id)
	if err != nil {
		return nil, err
	}
	if rt.Kind != registry.KindKeyManager {
		return nil, fmt.Errorf("keymanager: runtime is not a key manager: %s", id)
	}
	return rt, nil
}

func runtimeAttestationKey(ctx *tmapi.Context, regState *registryState.MutableState, kmRt *registry.Runtime) (*signature.PublicKey, error) {
	// Ensure that the signer is a key manager.
	n, err := regState.Node(ctx, ctx.TxSigner())
	if err != nil {
		return nil, err
	}
	idx := slices.IndexFunc(n.Runtimes, func(rt *node.Runtime) bool {
		// Skipping version check as key managers are running exactly one
		// version of the runtime.
		return rt.ID == kmRt.ID
	})
	if idx == -1 {
		return nil, fmt.Errorf("keymanager: node is not a key manager")
	}
	nRt := n.Runtimes[idx]

	// Fetch RAK. Remember that registration ensures that node's hardware meets
	// the TEE requirements of the key manager runtime.
	var rak *signature.PublicKey
	switch kmRt.TEEHardware {
	case node.TEEHardwareInvalid:
		rak = &api.InsecureRAK
	case node.TEEHardwareIntelSGX:
		if nRt.Capabilities.TEE == nil {
			return nil, fmt.Errorf("keymanager: node doesn't have TEE capability")
		}
		rak = &nRt.Capabilities.TEE.RAK
	default:
		return nil, fmt.Errorf("keymanager: TEE hardware mismatch")
	}

	return rak, nil
}

func runtimeEncryptionKeys(ctx *tmapi.Context, regState *registryState.MutableState, kmRt *registry.Runtime, kmStatus *secrets.Status) map[x25519.PublicKey]struct{} {
	// Fetch REKs of the key manager committee.
	reks := make(map[x25519.PublicKey]struct{})
	for _, id := range kmStatus.Nodes {
		n, err := regState.Node(ctx, id)
		if err != nil {
			continue
		}

		idx := slices.IndexFunc(n.Runtimes, func(rt *node.Runtime) bool {
			// Skipping version check as key managers are running exactly one
			// version of the runtime.
			return rt.ID == kmRt.ID
		})
		if idx == -1 {
			continue
		}
		nRt := n.Runtimes[idx]

		var rek x25519.PublicKey
		switch kmRt.TEEHardware {
		case node.TEEHardwareInvalid:
			rek = api.InsecureREK
		case node.TEEHardwareIntelSGX:
			if nRt.Capabilities.TEE == nil || nRt.Capabilities.TEE.REK == nil {
				continue
			}
			rek = *nRt.Capabilities.TEE.REK
		default:
			continue
		}

		reks[rek] = struct{}{}
	}

	return reks
}
