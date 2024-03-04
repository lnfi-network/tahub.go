package service

import (
	"context"
	b64 "encoding/base64"
	"encoding/hex"
	"fmt"
	"github.com/lightninglabs/taproot-assets/taprpc"
)

type AssetRoot struct {
	AssetName  string `json:"asset_name"`
	AssetID    string `json:"asset_id"`
	GroupKey   string `json:"group_key"`  
}

// TODO most of this logic was moved to the universe_to_assets go migration
//      abstract that to a utility function
func (svc *LndhubService) GetUniverseAssets(ctx context.Context) (okMsg string, success bool) {
	// req := universerpc.AssetRootRequest{}
	// universeRoots, err := svc.TapdClient.GetUniverseAssets(ctx, &req)
	universeRoots, err := svc.GetAssets(ctx)
	if err != nil {
		// TODO OK Relay-Compatible messages need a central location
		return "error: no assets found, possible disconnect.", false
	}
	var okSuccessMsg = "uniassets: "
	// since there can be two root entries per asset (one for issuance and one for transfer: https://lightning.engineering/api-docs/api/taproot-assets/universe/query-asset-roots#universerpcqueryrootresponse)
	// the observedAssetIds array helps us return something the user expects to see i.e. joins asset/transfer entry if both exist
	
	//var observedAssetIds = []string{}

	// TODO confirm when the key may be the group key hash instead of the assetId

	// for assetId, root := range universeRoots.UniverseRoots {
	// 	rawAssetId := strings.Split(assetId, "-")[1]
	// 	seen := slices.Contains(observedAssetIds, rawAssetId)

	// 	if !seen {
	// 		decoded, err := hex.DecodeString(rawAssetId)

	// 		if err != nil {
	// 			// TODO OK Relay-Compatible messages need a central location
	// 			return "error: failed to parse assetID.", false				
	// 		}

	// 		final := b64.StdEncoding.EncodeToString(decoded)

	// 		appendAsset := fmt.Sprintf("%s %s,", final, root.AssetName)
	// 		okSuccessMsg = okSuccessMsg + appendAsset

	// 		observedAssetIds = append(observedAssetIds, rawAssetId)
	// 	}
	// }
	for _, asset := range universeRoots {
		appendAsset := fmt.Sprintf("%s %s,", asset.TaAssetID, asset.AssetName)

		okSuccessMsg = okSuccessMsg + appendAsset
	}

	return okSuccessMsg, true
}

func  (svc *LndhubService) BalanceByAsset(ctx context.Context) (okMsg string, success bool) {
	/// * TODO this is a placeholder for now use account_ledger population on RecieveNotification Subscription
	filter := taprpc.ListBalancesRequest_AssetId{AssetId: true}
	req := taprpc.ListBalancesRequest{GroupBy: &filter}
	balances, err := svc.TapdClient.ListBalances(ctx, &req)
	if err != nil {
		// TODO OK Relay-Compatible messages need a central location
		return "error: failed to fetch balances.", false
	}
	aggBalances := make(map[string]uint64)
	var okSuccessMsg = "balances: "
	for _, balance := range balances.AssetBalances {

		// seen a group of this asset already
		bal, ok := aggBalances[balance.AssetGenesis.Name]
		if ok {
			aggBalances[balance.AssetGenesis.Name] = bal + balance.Balance
		} else {
			// add to map
			name := balance.AssetGenesis.Name
			aggBalances[name] = balance.Balance
		}
	}
	// check for no len
	if len(aggBalances) == 0 {
		// TODO OK Relay-Compatible messages need a central location
		return "balance: 0", false
	}
	// success message 
	for asset, balance := range aggBalances {
		okSuccessMsg = okSuccessMsg + fmt.Sprintf("%s %d,", asset, balance)
	}
	return okSuccessMsg, true
}

func (svc *LndhubService) GetAddressByAssetId(ctx context.Context, assetId string, amt uint64) (okMsg string, success bool) {
	decoded, err := b64.StdEncoding.DecodeString(assetId)
	if err != nil {
		// TODO OK Relay-Compatible messages need a central location
		return "error: failed to parse assetID.", false	
	}

	req := taprpc.NewAddrRequest{
		AssetId: decoded,
		Amt: amt,
	}
	newAddr, err := svc.TapdClient.NewAddress(ctx, &req)
	if err != nil {
		// TODO OK Relay-Compatible messages need a central location
		return "error: failed to create receive address.", false
	}
	return fmt.Sprintf("address: %s", newAddr.Encoded), true
}

func (svc *LndhubService) TransferAssets(ctx context.Context, userId uint64, addr string) (string, bool) {
	// decode addr
	req := taprpc.DecodeAddrRequest{Addr: addr}
	_, err := svc.TapdClient.GetDecodedAddress(ctx, &req)
	if err != nil {
		// TODO OK Relay-Compatible messages need a central location
		return "error: failed to decode address.", false
	}
	// check amount
	// TODO implement once receiver subscription is inserting transaction entries
	var hasFunding = true
	//sendAmt := decodedAddr.Amount
	//sendAssetId := hex.EncodeToString(decodedAddr.AssetId)
	if !hasFunding {
		// TODO OK Relay-Compatible messages need a central location
		return "error: insufficient funds.", false
	}
	// send asset
	// TODO estimate fee rate
	sendReq := taprpc.SendAssetRequest{
		TapAddrs: []string{addr},
	}
	_, err = svc.TapdClient.SendAsset(ctx, &sendReq)
	if err != nil {
		// TODO OK Relay-Compatible messages need a central location
		return "error: failed to send asset.", false
	}
	// return success message
	return "success: asset sent.", true
}

func (svc *LndhubService) FetchOrCreateAssetAddr(ctx context.Context, userId uint64, assetId string, amt uint64) (string, error) {
	// this is aware of amount so we can return early if an existing address is found
	addr, err := svc.FindAddress(ctx, userId, assetId, amt)
	// check db error - the nil check on addr indicates the error was on not found
	if err != nil && addr != nil {
		return "error: failed to check on existing address.", err
	}
	if addr != nil {
		// return existing address early
		return fmt.Sprintf("address: %s", addr.Addr), nil
	}
	// decode assetId for tapd request
	decoded, err := hex.DecodeString(assetId)
	if err != nil {
		// TODO OK Relay-Compatible messages need a central location
		return "error: failed to parse assetID.", err	
	}
	// create new address
	req := taprpc.NewAddrRequest{
		AssetId: decoded,
		Amt: amt,
	}
	newAddr, err := svc.TapdClient.NewAddress(ctx, &req)
	if err != nil {
		// TODO OK Relay-Compatible messages need a central location
		return "error: failed to create receive address.", err
	}
	// save new address to db
	_, err = svc.CreateAddress(ctx, newAddr.Encoded, userId, assetId, amt)
	if err != nil {
		// TODO OK Relay-Compatible messages need a central location
		return "error: failed to save receive address.", err
	}
	// return success message
	return fmt.Sprintf("address: %s", newAddr.Encoded), nil
}