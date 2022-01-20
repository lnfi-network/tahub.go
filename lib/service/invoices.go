package service

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"math/rand"
	"strings"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/getAlby/lndhub.go/db/models"
	"github.com/labstack/gommon/random"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/zpay32"
	"github.com/uptrace/bun/schema"
)

func (svc *LndhubService) FindInvoiceByPaymentHash(userId int64, rHash string) (*models.Invoice, error) {
	var invoice models.Invoice

	err := svc.DB.NewSelect().Model(&invoice).Where("invoice.user_id = ? AND invoice.r_hash = ?", userId, rHash).Limit(1).Scan(context.TODO())
	if err != nil {
		return &invoice, err
	}
	return &invoice, nil
}

func (svc *LndhubService) PayInvoice(invoice *models.Invoice) (*models.TransactionEntry, error) {
	userId := invoice.UserID

	// Get the user's current and outgoing account for the transaction entry
	debitAccount, err := svc.AccountFor(context.TODO(), "current", userId)
	if err != nil {
		return nil, err
	}
	creditAccount, err := svc.AccountFor(context.TODO(), "outgoing", userId)
	if err != nil {
		return nil, err
	}

	entry := models.TransactionEntry{
		UserID:          userId,
		InvoiceID:       invoice.ID,
		CreditAccountID: creditAccount.ID,
		DebitAccountID:  debitAccount.ID,
		Amount:          invoice.Amount,
	}

	// Start a DB transaction
	// We rollback anything on error (only the invoice that was passed in to the PayInvoice calls stays in the DB)
	tx, err := svc.DB.BeginTx(context.TODO(), &sql.TxOptions{})
	if err != nil {
		return &entry, err
	}

	// The DB constraints make sure the user actually has enough balance for the transaction
	// If the user does not have enough balance this call fails
	_, err = tx.NewInsert().Model(&entry).Exec(context.TODO())
	if err != nil {
		tx.Rollback()
		return &entry, err
	}

	// TODO: set fee limit
	feeLimit := lnrpc.FeeLimit{
		Limit: &lnrpc.FeeLimit_Percent{
			Percent: 2,
		},
	}

	// Prepare the LNRPC call
	sendPaymentRequest := lnrpc.SendRequest{
		PaymentRequest: invoice.PaymentRequest,
		Amt:            invoice.Amount,
		FeeLimit:       &feeLimit,
	}

	// Execute the payment
	sendPaymentResult, err := svc.LndClient.SendPaymentSync(context.TODO(), &sendPaymentRequest)
	if err != nil {
		tx.Rollback()
		return &entry, err
	}

	// If there was a payment error we rollback and return an error
	if sendPaymentResult.GetPaymentError() != "" || sendPaymentResult.GetPaymentPreimage() == nil {
		tx.Rollback()
		return &entry, errors.New(sendPaymentResult.GetPaymentError())
	}

	// The payment was successful.
	// We store the preimage and mark the invoice as settled
	preimage := sendPaymentResult.GetPaymentPreimage()
	invoice.Preimage = hex.EncodeToString(preimage[:])
	invoice.State = "settled"
	invoice.SettledAt = schema.NullTime{Time: time.Now()}

	_, err = svc.DB.NewUpdate().Model(invoice).WherePK().Exec(context.TODO())
	if err != nil {
		tx.Rollback()
		return &entry, err
	}

	// Commit the DB transaction. Done, everything worked
	err = tx.Commit()
	if err != nil {
		return &entry, err
	}

	return &entry, err
}

func (svc *LndhubService) AddOutgoingInvoice(userID int64, paymentRequest string, decodedInvoice zpay32.Invoice) (*models.Invoice, error) {
	// Initialize new DB invoice
	destinationPubkey := hex.EncodeToString(decodedInvoice.Destination.SerializeCompressed())
	invoice := models.Invoice{
		Type:              "outgoing",
		UserID:            userID,
		Memo:              *decodedInvoice.Description,
		PaymentRequest:    paymentRequest,
		State:             "initialized",
		DestinationPubKey: destinationPubkey,
	}
	if decodedInvoice.DescriptionHash != nil {
		dh := *decodedInvoice.DescriptionHash
		invoice.DescriptionHash = hex.EncodeToString(dh[:])
	}
	if decodedInvoice.PaymentHash != nil {
		ph := *decodedInvoice.PaymentHash
		invoice.RHash = hex.EncodeToString(ph[:])
	}
	if decodedInvoice.MilliSat != nil {
		msat := decodedInvoice.MilliSat
		invoice.Amount = int64(msat.ToSatoshis())
	}

	// Save invoice
	_, err := svc.DB.NewInsert().Model(&invoice).Exec(context.TODO())
	if err != nil {
		return nil, err
	}
	return &invoice, nil
}

func (svc *LndhubService) AddIncomingInvoice(userID int64, amount int64, memo, descriptionHash string) (*models.Invoice, error) {
	// Initialize new DB invoice
	invoice := models.Invoice{
		Type:            "incoming",
		UserID:          userID,
		Amount:          amount,
		Memo:            memo,
		DescriptionHash: descriptionHash,
		State:           "initialized",
	}

	// Save invoice - we save the invoice early to have a record in case the LN call fails
	_, err := svc.DB.NewInsert().Model(&invoice).Exec(context.TODO())
	if err != nil {
		return nil, err
	}

	// Initialize lnrpc invoice
	lnInvoice := lnrpc.Invoice{
		Memo:      memo,
		Value:     amount,
		RPreimage: makePreimageHex(),
		Expiry:    3600 * 24, // 24h
	}
	// Call LND
	lnInvoiceResult, err := svc.LndClient.AddInvoice(context.TODO(), &lnInvoice)
	if err != nil {
		return nil, err
	}

	// Update the DB invoice with the data from the LND gRPC call
	invoice.PaymentRequest = lnInvoiceResult.PaymentRequest
	invoice.RHash = hex.EncodeToString(lnInvoiceResult.RHash)
	invoice.AddIndex = lnInvoiceResult.AddIndex
	invoice.DestinationPubKey = svc.GetIdentPubKeyHex() // Our node pubkey for incoming invoices
	invoice.State = "created"

	_, err = svc.DB.NewUpdate().Model(&invoice).WherePK().Exec(context.TODO())
	if err != nil {
		return nil, err
	}

	return &invoice, nil
}

func (svc *LndhubService) DecodePaymentRequest(bolt11 string) (*zpay32.Invoice, error) {
	return zpay32.Decode(bolt11, ChainFromCurrency(bolt11[2:]))
}

const hexBytes = random.Hex

func makePreimageHex() []byte {
	b := make([]byte, 32)
	for i := range b {
		b[i] = hexBytes[rand.Intn(len(hexBytes))]
	}
	return b
}

func ChainFromCurrency(currency string) *chaincfg.Params {
	if strings.HasPrefix(currency, "bcrt") {
		return &chaincfg.RegressionNetParams
	} else if strings.HasPrefix(currency, "tb") {
		return &chaincfg.TestNet3Params
	} else if strings.HasPrefix(currency, "sb") {
		return &chaincfg.SimNetParams
	} else {
		return &chaincfg.MainNetParams
	}
}
