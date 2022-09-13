package keeper

import (
	"fmt"

	"github.com/tendermint/tendermint/libs/log"

	"github.com/CudoVentures/cudos-node/x/marketplace/types"
	"github.com/CudoVentures/cudos-node/x/nft/exported"
	"github.com/cosmos/cosmos-sdk/codec"
	sdk "github.com/cosmos/cosmos-sdk/types"
	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"
	paramtypes "github.com/cosmos/cosmos-sdk/x/params/types"
)

type (
	Keeper struct {
		cdc        codec.BinaryCodec
		storeKey   sdk.StoreKey
		memKey     sdk.StoreKey
		paramstore paramtypes.Subspace

		bankKeeper types.BankKeeper
		nftKeeper  types.NftKeeper
	}
)

func NewKeeper(
	cdc codec.BinaryCodec,
	storeKey,
	memKey sdk.StoreKey,
	ps paramtypes.Subspace,

	bankKeeper types.BankKeeper, nftKeeper types.NftKeeper,
) *Keeper {
	// set KeyTable if it has not already been set
	if !ps.HasKeyTable() {
		ps = ps.WithKeyTable(types.ParamKeyTable())
	}

	return &Keeper{

		cdc:        cdc,
		storeKey:   storeKey,
		memKey:     memKey,
		paramstore: ps,
		bankKeeper: bankKeeper, nftKeeper: nftKeeper,
	}
}

func (k Keeper) Logger(ctx sdk.Context) log.Logger {
	return ctx.Logger().With("module", fmt.Sprintf("x/%s", types.ModuleName))
}

func (k Keeper) isCollectionPublished(ctx sdk.Context, denomID string) bool {
	store := ctx.KVStore(k.storeKey)
	return store.Has(types.KeyCollectionDenomID(denomID))
}

func (k Keeper) PublishCollection(ctx sdk.Context, collection types.Collection) (uint64, error) {
	denom, err := k.nftKeeper.GetDenom(ctx, collection.DenomId)
	if err != nil {
		return 0, err
	}

	if denom.Creator != collection.Owner {
		return 0, sdkerrors.Wrapf(types.ErrNotDenomOwner, "Owner of denom %s is %s", collection.DenomId, collection.Owner)
	}

	if k.isCollectionPublished(ctx, collection.DenomId) {
		return 0, sdkerrors.Wrapf(types.ErrCollectionAlreadyPublished, "Collection for denom %s is already published", collection.DenomId)
	}

	collectionID := k.AppendCollection(ctx, collection)

	store := ctx.KVStore(k.storeKey)
	store.Set(types.KeyCollectionDenomID(collection.DenomId), types.Uint64ToBytes(collectionID))

	return collectionID, nil
}

func (k Keeper) isApprovedNftAddress(nftVal exported.NFT, owner string) bool {
	approvedAddresses := nftVal.GetApprovedAddresses()
	for _, addr := range approvedAddresses {
		if addr == owner {
			return true
		}
	}
	return false
}

func (k Keeper) PublishNFT(ctx sdk.Context, nft types.Nft) (uint64, error) {
	if _, err := k.nftKeeper.GetDenom(ctx, nft.DenomId); err != nil {
		return 0, err
	}

	nftVal, err := k.nftKeeper.GetNFT(ctx, nft.DenomId, nft.TokenId)
	if err != nil {
		return 0, err
	}

	if nftVal.GetOwner().String() == nft.Owner ||
		k.nftKeeper.IsApprovedOperator(ctx, nftVal.GetOwner(), sdk.AccAddress(nft.Owner)) ||
		k.isApprovedNftAddress(nftVal, nft.Owner) {

		store := ctx.KVStore(k.storeKey)
		key := types.KeyNftDenomTokenID(nft.DenomId, nft.TokenId)
		if b := store.Get(key); len(b) > 0 {
			return 0, sdkerrors.Wrapf(types.ErrNftAlreadyPublished, "nft with token id (%s) from denom (%s) already published for sale", nft.TokenId, nft.DenomId)
		}

		if err := k.nftKeeper.SoftLockNFT(ctx, types.ModuleName, nft.DenomId, nft.TokenId); err != nil {
			return 0, err
		}

		nftID := k.AppendNft(ctx, nft)

		store.Set(key, types.Uint64ToBytes(nftID))

		return nftID, nil
	}

	return 0, sdkerrors.Wrapf(types.ErrNotNftOwner, "%s not nft owner or approved operator for token id (%s) from denom (%s)", nft.Owner, nft.TokenId, nft.DenomId)
}

func (k Keeper) BuyNFT(ctx sdk.Context, nftID uint64, buyer sdk.AccAddress) error {
	nft, found := k.GetNft(ctx, nftID)
	if !found {
		return sdkerrors.Wrapf(types.ErrNftNotFound, "nft with id (%d) is not found for sale", nftID)
	}

	if nft.Owner == buyer.String() {
		return sdkerrors.Wrap(types.ErrCannotBuyOwnNft, "cannot buy own nft")
	}

	if err := k.bankKeeper.SendCoinsFromAccountToModule(ctx, buyer, types.ModuleName, sdk.NewCoins(nft.Price)); err != nil {
		return err
	}

	collection, found := k.getCollectionByDenomID(ctx, nft.DenomId)
	if !found || len(collection.ResaleRoyalties) == 0 {

		sellerAddr, err := sdk.AccAddressFromBech32(nft.Owner)
		if err != nil {
			return sdkerrors.Wrapf(sdkerrors.ErrInvalidAddress, "invalid seller address (%s): %s", nft.Owner, err)
		}

		if err := k.bankKeeper.SendCoinsFromModuleToAccount(ctx, types.ModuleName, sellerAddr, sdk.NewCoins(nft.Price)); err != nil {
			return err
		}
	}

	if err := k.DistributeRoyalties(ctx, nft.Price, nft.Owner, collection.ResaleRoyalties); err != nil {
		return err
	}

	store := ctx.KVStore(k.storeKey)
	store.Delete(types.KeyNftDenomTokenID(nft.DenomId, nft.TokenId))

	k.RemoveNft(ctx, nftID)

	baseNft, err := k.nftKeeper.GetBaseNFT(ctx, nft.DenomId, nft.TokenId)
	if err != nil {
		return err
	}

	if err := k.nftKeeper.SoftUnlockNFT(ctx, types.ModuleName, nft.DenomId, nft.TokenId); err != nil {
		return err
	}

	k.nftKeeper.TransferNftInternal(ctx, nft.DenomId, nft.TokenId, sdk.AccAddress(nft.Owner), buyer, baseNft)

	return nil
}

func (k Keeper) MintNFT(ctx sdk.Context, denomID, name, uri, data string, price sdk.Coin, recipient sdk.AccAddress, sender sdk.AccAddress) (string, error) {
	denom, err := k.nftKeeper.GetDenom(ctx, denomID)
	if err != nil {
		return "", err
	}

	collection, found := k.getCollectionByDenomID(ctx, denomID)
	if !found {
		return "", sdkerrors.Wrapf(types.ErrCollectionNotFound, "collection %s not published for sale", denomID)
	}

	if err := k.DistributeRoyalties(ctx, price, denom.Creator, collection.MintRoyalties); err != nil {
		return "", err
	}

	return k.nftKeeper.MintNFT(ctx, denomID, name, uri, data, sender, recipient)
}

func (k Keeper) RemoveNFT(ctx sdk.Context, nftID uint64, owner sdk.AccAddress) error {
	nft, found := k.GetNft(ctx, nftID)
	if !found {
		return sdkerrors.Wrapf(types.ErrNftNotFound, "nft with id (%d) is not found for sale", nftID)
	}

	if nft.Owner != owner.String() {
		return sdkerrors.Wrapf(types.ErrNotNftOwner, "not owner of (%d)", nftID)
	}

	store := ctx.KVStore(k.storeKey)
	store.Delete(types.KeyNftDenomTokenID(nft.DenomId, nft.TokenId))

	k.RemoveNft(ctx, nftID)

	if err := k.nftKeeper.SoftUnlockNFT(ctx, types.ModuleName, nft.DenomId, nft.TokenId); err != nil {
		return err
	}

	return nil
}

func (k Keeper) getCollectionByDenomID(ctx sdk.Context, denomID string) (types.Collection, bool) {
	store := ctx.KVStore(k.storeKey)
	collectionIDBytes := store.Get(types.KeyCollectionDenomID(denomID))
	if collectionIDBytes == nil {
		return types.Collection{}, false
	}
	return k.GetCollection(ctx, types.BytesToUint64(collectionIDBytes))
}

func getProportion(totalCoin sdk.Coin, ratio sdk.Dec) sdk.Coin {
	return sdk.NewCoin(totalCoin.Denom, totalCoin.Amount.ToDec().Mul(ratio).Quo(sdk.NewDec(100)).TruncateInt())
}

func (k Keeper) DistributeRoyalties(ctx sdk.Context, price sdk.Coin, seller string, royalties []types.Royalty) error {
	if len(royalties) == 0 {
		return nil
	}

	amountLeft := price.Amount

	for _, royalty := range royalties {

		royaltyReceiver, err := sdk.AccAddressFromBech32(royalty.Address)
		if err != nil {
			return sdkerrors.Wrapf(sdkerrors.ErrInvalidAddress, "invalid royalty address (%s): %s", royalty.Address, err)
		}

		portion := getProportion(price, royalty.Percent)
		amountLeft = amountLeft.Sub(portion.Amount)

		if err := k.bankKeeper.SendCoinsFromModuleToAccount(ctx, types.ModuleName, royaltyReceiver, sdk.NewCoins(portion)); err != nil {
			return err
		}
	}

	if amountLeft.GT(sdk.NewInt(0)) {
		sellerAddr, err := sdk.AccAddressFromBech32(seller)
		if err != nil {
			return sdkerrors.Wrapf(sdkerrors.ErrInvalidAddress, "invalid seller address (%s): %s", seller, err)
		}

		return k.bankKeeper.SendCoinsFromModuleToAccount(ctx, types.ModuleName, sellerAddr, sdk.NewCoins(sdk.NewCoin(price.Denom, amountLeft)))
	}

	return nil
}

func (k Keeper) getCollectionStatus(ctx sdk.Context, id uint64) (bool, error) {
	collection, found := k.GetCollection(ctx, id)
	if !found {
		return false, sdkerrors.Wrapf(types.ErrCollectionNotFound, "collection with id %d not found", id)
	}
	return collection.Verified, nil
}

func (k Keeper) setCollectionStatus(ctx sdk.Context, id uint64, verified bool) error {
	collection, found := k.GetCollection(ctx, id)
	if !found {
		return sdkerrors.Wrapf(types.ErrCollectionNotFound, "collection with id %d not found", id)
	}
	collection.Verified = verified
	k.SetCollection(ctx, collection)
	return nil
}
