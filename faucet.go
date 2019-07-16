package main

import (
	"crypto/x509"
	"fmt"
	"html/template"
	"io/ioutil"
	"net/http"
	"strconv"
	"sync"
	"time"

	macaroon "gopkg.in/macaroon.v2"

	"github.com/decred/dcrd/dcrutil"
	"github.com/decred/dcrlnd/lnrpc"
	"github.com/decred/dcrlnd/macaroons"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

const (
	// maxChannelSize is the larget channel that the faucet will create to
	// another peer.
	maxChannelSize int64 = (1 << 30)

	// minChannelSize is the smallest channel that the faucet will extend
	// to a peer.
	minChannelSize int64 = 50000
)

// chanCreationError is an enum which describes the exact nature of an error
// encountered when a user attempts to create a channel with the faucet. This
// enum is used within the templates to determine at which input item the error
// occurred  and also to generate an error string unique to the error.
type chanCreationError uint8

const (
	// NoError is the default error which indicates either the form hasn't
	// yet been submitted or no errors have arisen.
	NoError chanCreationError = iota

	// InvalidAddress indicates that the passed node address is invalid.
	InvalidAddress

	// NotConnected indicates that the target peer isn't connected to the
	// faucet.
	NotConnected

	// ChanAmountNotNumber indicates that the amount specified for the
	// amount to fund the channel with isn't actually a number.
	ChanAmountNotNumber

	// ChannelTooLarge indicates that the amounts specified to fund the
	// channel with is greater than maxChannelSize.
	ChannelTooLarge

	// ChannelTooSmall indicates that the channel size required is below
	// minChannelSize.
	ChannelTooSmall

	// PushIncorrect indicates that the amount specified to push to the
	// other end of the channel is greater-than-or-equal-to the local
	// funding amount.
	PushIncorrect

	// ChannelOpenFail indicates some error occurred when attempting to
	// open a channel with the target peer.
	ChannelOpenFail

	// HaveChannel indicates that the faucet already has a channel open
	// with the target node.
	HaveChannel

	// HavePendingChannel indicates that the faucet already has a channel
	// pending with the target node.
	HavePendingChannel

	// ErrorGeneratingInvoice indicates that some error happened when generating
	// an invoice
	ErrorGeneratingInvoice

	// InvoiceTimeNotElapsed indicates minimum time to create a new invoice has not elapsed
	InvoiceTimeNotElapsed

	// InvoiceAmountTooHigh indicates the user tried to generate an invoice
	// that was too expensive.
	InvoiceAmountTooHigh
)

var (

	// GenerateInvoiceTimeout represents the minimum time to generate a new
	// invoice in seconds.
	GenerateInvoiceTimeout = time.Duration(60) * time.Second

	// GenerateInvoiceAction represents an action to generate invoice on post forms
	GenerateInvoiceAction = "generateinvoice"
)

// lastGeneratedInvoiceTime stores the last time an invoice generation was
// attempted.
var lastGeneratedInvoiceTime time.Time

// String returns a human readable string describing the chanCreationError.
// This string is used in the templates in order to display the error to the
// user.
func (c chanCreationError) String() string {
	switch c {
	case NoError:
		return ""
	case InvalidAddress:
		return "Not a valid public key"
	case NotConnected:
		return "Faucet cannot connect to this node"
	case ChanAmountNotNumber:
		return "Amount must be a number"
	case ChannelTooLarge:
		return "Amount is too large"
	case ChannelTooSmall:
		return fmt.Sprintf("Minimum channel size is is %d DCR", minChannelSize)
	case PushIncorrect:
		return "Initial Balance is incorrect"
	case ChannelOpenFail:
		return "Faucet is not able to open a channel with this node"
	case HaveChannel:
		return "Faucet already has an active channel with this node"
	case HavePendingChannel:
		return "Faucet already has a pending channel with this node"
	case ErrorGeneratingInvoice:
		return "Error generating Invoice"
	case InvoiceTimeNotElapsed:
		return "Please wait until you can generate a new invoice"
	case InvoiceAmountTooHigh:
		return "Invoice amount too high"
	default:
		return fmt.Sprintf("%v", uint8(c))
	}
}

// lightningFaucet is a Decred Channel Faucet. The faucet itself is a web app
// that is capable of programmatically opening channels with users with the
// size of the channel parametrized by the user. The faucet required a
// connection to a local lnd node in order to operate properly. The faucet
// implements the constrains on the channel size, and also will only open a
// single channel to a particular node. Finally, the faucet will periodically
// close channels based on their age as the faucet will only open up 100
// channels total at any given time.
type lightningFaucet struct {
	lnd lnrpc.LightningClient

	templates       *template.Template
	homePageContext *homePageContext

	openChanMtx sync.RWMutex
}

func newTemplate(templates *template.Template) *lightningFaucet {
	return &lightningFaucet{
		templates: templates,
		homePageContext: &homePageContext{
			FormFields:            make(map[string]string),
			GenerateInvoiceAction: GenerateInvoiceAction,
		},
	}
}

// newLightningClient creates a new channel faucet that's bound to a cluster of
// lnd nodes, and uses the passed templates to render the web page.
func newLightningClient(lndNode string, tlsCert *x509.CertPool, macBytes []byte, l *lightningFaucet) error {

	// First attempt to establish a connection to lnd's RPC sever.
	creds := credentials.NewClientTLSFromCert(tlsCert, "")
	opts := []grpc.DialOption{grpc.WithTransportCredentials(creds)}

	// Load the specified macaroon file.
	mac := &macaroon.Macaroon{}
	if err := mac.UnmarshalBinary(macBytes); err != nil {
		return err
	}

	// Now we append the macaroon credentials to the dial options.
	opts = append(
		opts,
		grpc.WithPerRPCCredentials(macaroons.NewMacaroonCredential(mac)),
	)

	// opts := grpc.WithPerRPCCredentials(macaroons.NewMacaroonCredential(mac))
	conn, err := grpc.Dial(lndNode, opts...)
	if err != nil {
		return fmt.Errorf("unable to dial to lnd's gRPC server: %v", err)
	}

	lnd := lnrpc.NewLightningClient(conn)

	l.lnd = lnd
	return nil
}

// homePageContext defines the initial context required for rendering home
// page. The home page displays some basic statistics, errors in the case of an
// invalid channel submission, and finally a splash page upon successful
// creation of a channel.
type homePageContext struct {
	// GitCommitHash is the git HEAD's commit hash of
	// $GOPATH/src/github.com/lightningnetwork/lnd
	GitCommitHash string

	// NodeAddr is the full <pubkey>@host:port where the faucet can be
	// connect to.
	NodeAddr string

	// SubmissionError is a enum that stores if any error took place during
	// the creation of a channel.
	SubmissionError chanCreationError

	// FormFields contains the values which were submitted through the form.
	FormFields map[string]string

	// InvoicePaymentRequest the payment request generated by an invoice.
	InvoicePaymentRequest string

	// GenerateInvoiceAction indicates the form action to generate a new Invoice
	GenerateInvoiceAction string
}

// faucetHome renders the main home page for the faucet. This includes the form
// to create channels, the network statistics, and the splash page upon channel
// success.
//
// NOTE: This method implements the http.Handler interface.
func (l *lightningFaucet) faucetHome(w http.ResponseWriter, r *http.Request) {
	// First obtain the home template from our cache of pre-compiled
	// templates.
	homeTemplate := l.templates.Lookup("index.html")

	// In order to render the home template we'll need the necessary
	// context, so we'll grab that from the lnd daemon now in order to get
	// the most up to date state.
	homeInfoContext := l.homePageContext

	// If the method is GET, then we'll render the home page with the form
	// itself.
	switch {
	case r.Method == http.MethodGet:
		homeTemplate.Execute(w, homeInfoContext)

	// Otherwise, if the method is POST, then the user is submitting the
	// form to open a channel, so we'll pass that off to the openChannel
	// handler.
	case r.Method == http.MethodPost:
		action, _ := r.URL.Query()["action"]

		if action[0] == GenerateInvoiceAction {
			l.generateInvoice(homeTemplate, homeInfoContext, w, r)
		}

	// If the method isn't either of those, then this is an error as we
	// only support the two methods above.
	default:
		http.Error(w, "", http.StatusMethodNotAllowed)
	}

	return
}

// generateInvoice is a hybrid http.Handler that handles: the validation of the
// generate invoice form, rendering errors to the form, and finally generating
// invoice if all the parameters check out.
func (l *lightningFaucet) generateInvoice(homeTemplate *template.Template,
	homeState *homePageContext, w http.ResponseWriter, r *http.Request) {

	elapsed := time.Since(lastGeneratedInvoiceTime)
	lndNode := r.FormValue("nodeurl")
	amt := r.FormValue("amt")
	description := r.FormValue("description")
	tlsCert, _, err := r.FormFile("tlscert")
	if err != nil {
		fmt.Println(err)
		return
	}
	// read all of the contents of our uploaded file into a
	// byte array
	fileBytes, err := ioutil.ReadAll(tlsCert)
	if err != nil {
		fmt.Println(err)
		return
	}
	cert := x509.NewCertPool()
	cert.AppendCertsFromPEM(fileBytes)
	defer tlsCert.Close()

	adminMacaroon, _, err := r.FormFile("adminmacaroon")
	if err != nil {
		fmt.Println(err)
		return
	}
	macBytes, err := ioutil.ReadAll(adminMacaroon)
	if err != nil {
		fmt.Println(err)
		return
	}
	defer adminMacaroon.Close()

	homeState.FormFields["LndNode"] = lndNode
	homeState.FormFields["Amt"] = amt
	homeState.FormFields["Description"] = description

	// check if minimium timeout to generate invoice has passed
	if elapsed < GenerateInvoiceTimeout {
		homeState.SubmissionError = InvoiceTimeNotElapsed
		homeTemplate.Execute(w, homeState)
		return
	}
	lastGeneratedInvoiceTime = time.Now()
	if err := r.ParseForm(); err != nil {
		http.Error(w, "unable to parse form", 500)
		return
	}

	amtDcr, err := strconv.ParseFloat(amt, 64)
	if err != nil {
		homeState.SubmissionError = ChanAmountNotNumber
		homeTemplate.Execute(w, homeState)
		return
	}
	if amtDcr > 0.2 {
		log.Warnf("Attempt to generate high value invoice (%f) from %s",
			amtDcr, r.RemoteAddr)
		homeState.SubmissionError = InvoiceAmountTooHigh
		homeTemplate.Execute(w, homeState)
		return
	}
	amtAtoms := int64(amtDcr * 1e8)

	// Create a new lightning client with the inputs submited
	err = newLightningClient(lndNode, cert, macBytes, l)
	if err != nil {
		log.Errorf("Generate invoice failed: %v", err)
		homeState.SubmissionError = ErrorGeneratingInvoice
		homeTemplate.Execute(w, homeState)
		return
	}

	// generate new invoice
	invoiceReq := &lnrpc.Invoice{
		CreationDate: time.Now().Unix(),
		Value:        amtAtoms,
		Memo:         description,
	}
	invoice, err := l.lnd.AddInvoice(ctxb, invoiceReq)
	if err != nil {
		log.Errorf("Generate invoice failed: %v", err)
		homeState.SubmissionError = ErrorGeneratingInvoice
		homeTemplate.Execute(w, homeState)
		return
	}

	log.Infof("Generated invoice #%d for %s rhash=%064x", invoice.AddIndex,
		dcrutil.Amount(amtAtoms), invoice.RHash)

	homeState.InvoicePaymentRequest = invoice.PaymentRequest

	if err := homeTemplate.Execute(w, homeState); err != nil {
		log.Errorf("unable to render home page: %v", err)
	}
}
