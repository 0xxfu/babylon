package keeper

import (
	"context"
	"fmt"

	"github.com/cosmos/cosmos-sdk/runtime"

	"cosmossdk.io/store/prefix"
	bbn "github.com/babylonchain/babylon/types"
	"github.com/babylonchain/babylon/x/btcstaking/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
)

// RecordVotingPowerTable computes the voting power table at the current block height
// and saves the power table to KVStore
// triggered upon each EndBlock
func (k Keeper) RecordVotingPowerTable(ctx context.Context) {
	covenantQuorum := k.GetParams(ctx).CovenantQuorum
	// tip of Babylon and Bitcoin
	babylonTipHeight := uint64(sdk.UnwrapSDKContext(ctx).BlockHeight())
	btcTipHeight, err := k.GetCurrentBTCHeight(ctx)
	if err != nil {
		return
	}
	// get value of w
	wValue := k.btccKeeper.GetParams(ctx).CheckpointFinalizationTimeout

	// filter out all BTC validators with positive voting power
	activeBTCVals := []*types.BTCValidatorWithMeta{}
	btcValIter := k.btcValidatorStore(ctx).Iterator(nil, nil)
	for ; btcValIter.Valid(); btcValIter.Next() {
		valBTCPKBytes := btcValIter.Key()
		valBTCPK, err := bbn.NewBIP340PubKey(valBTCPKBytes)
		if err != nil {
			// failed to unmarshal BTC validator PK in KVStore is a programming error
			panic(err)
		}
		btcVal, err := k.GetBTCValidator(ctx, valBTCPKBytes)
		if err != nil {
			// failed to get a BTC validator with voting power is a programming error
			panic(err)
		}
		if btcVal.IsSlashed() {
			// slashed BTC validator is removed from BTC validator set
			continue
		}

		valPower := uint64(0)

		// iterate all BTC delegations under this validator
		// to calculate this validator's total voting power
		btcDelIter := k.btcDelegatorStore(ctx, valBTCPK).Iterator(nil, nil)
		for ; btcDelIter.Valid(); btcDelIter.Next() {
			delBTCPK, err := bbn.NewBIP340PubKey(btcDelIter.Key())
			if err != nil {
				panic(err) // only programming error is possible
			}
			btcDels, err := k.getBTCDelegatorDelegations(ctx, valBTCPK, delBTCPK)
			if err != nil {
				panic(err) // only programming error is possible
			}
			valPower += btcDels.VotingPower(btcTipHeight, wValue, covenantQuorum)
		}
		btcDelIter.Close()

		if valPower > 0 {
			activeBTCVals = append(activeBTCVals, &types.BTCValidatorWithMeta{
				BtcPk:       valBTCPK,
				VotingPower: valPower,
				// other fields do not matter
			})
		}
	}
	btcValIter.Close()

	// return directly if there is no active BTC validator
	if len(activeBTCVals) == 0 {
		return
	}

	// filter out top `MaxActiveBtcValidators` active validators in terms of voting power
	activeBTCVals = types.FilterTopNBTCValidators(activeBTCVals, k.GetParams(ctx).MaxActiveBtcValidators)

	// set voting power for each active BTC validators
	for _, btcVal := range activeBTCVals {
		k.SetVotingPower(ctx, btcVal.BtcPk.MustMarshal(), babylonTipHeight, btcVal.VotingPower)
	}
}

// SetVotingPower sets the voting power of a given BTC validator at a given Babylon height
func (k Keeper) SetVotingPower(ctx context.Context, valBTCPK []byte, height uint64, power uint64) {
	store := k.votingPowerStore(ctx, height)
	store.Set(valBTCPK, sdk.Uint64ToBigEndian(power))
}

// GetVotingPower gets the voting power of a given BTC validator at a given Babylon height
func (k Keeper) GetVotingPower(ctx context.Context, valBTCPK []byte, height uint64) uint64 {
	if !k.HasBTCValidator(ctx, valBTCPK) {
		return 0
	}
	store := k.votingPowerStore(ctx, height)
	powerBytes := store.Get(valBTCPK)
	if len(powerBytes) == 0 {
		return 0
	}
	return sdk.BigEndianToUint64(powerBytes)
}

// HasVotingPowerTable checks if the voting power table exists at a given height
func (k Keeper) HasVotingPowerTable(ctx context.Context, height uint64) bool {
	store := k.votingPowerStore(ctx, height)
	iter := store.Iterator(nil, nil)
	defer iter.Close()
	return iter.Valid()
}

// GetVotingPowerTable gets the voting power table, i.e., validator set at a given height
func (k Keeper) GetVotingPowerTable(ctx context.Context, height uint64) map[string]uint64 {
	store := k.votingPowerStore(ctx, height)
	iter := store.Iterator(nil, nil)
	defer iter.Close()

	// if no validator at this height, return nil
	if !iter.Valid() {
		return nil
	}

	// get all validators at this height
	valSet := map[string]uint64{}
	for ; iter.Valid(); iter.Next() {
		valBTCPK, err := bbn.NewBIP340PubKey(iter.Key())
		if err != nil {
			// failing to unmarshal validator BTC PK in KVStore is a programming error
			panic(fmt.Errorf("%w: %w", bbn.ErrUnmarshal, err))
		}
		valSet[valBTCPK.MarshalHex()] = sdk.BigEndianToUint64(iter.Value())
	}

	return valSet
}

// GetBTCStakingActivatedHeight returns the height when the BTC staking protocol is activated
// i.e., the first height where a BTC validator has voting power
// Before the BTC staking protocol is activated, we don't index or tally any block
func (k Keeper) GetBTCStakingActivatedHeight(ctx context.Context) (uint64, error) {
	storeAdapter := runtime.KVStoreAdapter(k.storeService.OpenKVStore(ctx))
	votingPowerStore := prefix.NewStore(storeAdapter, types.VotingPowerKey)
	iter := votingPowerStore.Iterator(nil, nil)
	defer iter.Close()
	// if the iterator is valid, then there exists a height that has a BTC validator with voting power
	if iter.Valid() {
		return sdk.BigEndianToUint64(iter.Key()), nil
	} else {
		return 0, types.ErrBTCStakingNotActivated
	}
}

func (k Keeper) IsBTCStakingActivated(ctx context.Context) bool {
	storeAdapter := runtime.KVStoreAdapter(k.storeService.OpenKVStore(ctx))
	votingPowerStore := prefix.NewStore(storeAdapter, types.VotingPowerKey)
	iter := votingPowerStore.Iterator(nil, nil)
	defer iter.Close()
	// if the iterator is valid, then BTC staking is already activated
	return iter.Valid()
}

// votingPowerStore returns the KVStore of the BTC validators' voting power
// prefix: (VotingPowerKey || Babylon block height)
// key: Bitcoin secp256k1 PK
// value: voting power quantified in Satoshi
func (k Keeper) votingPowerStore(ctx context.Context, height uint64) prefix.Store {
	storeAdapter := runtime.KVStoreAdapter(k.storeService.OpenKVStore(ctx))
	votingPowerStore := prefix.NewStore(storeAdapter, types.VotingPowerKey)
	return prefix.NewStore(votingPowerStore, sdk.Uint64ToBigEndian(height))
}
