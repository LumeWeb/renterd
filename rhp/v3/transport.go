package rhp

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"strings"
	"sync"

	"gitlab.com/NebulousLabs/encoding"
	"go.sia.tech/core/types"
	"go.sia.tech/mux"
	"lukechampine.com/frand"
)

func wrapErr(err *error, fnName string) {
	if *err != nil {
		*err = fmt.Errorf("%s: %w", fnName, *err)
	}
}

// PriceTablePaymentFunc is a function that can be passed in to RPCPriceTable.
// It is called after the price table is received from the host and supposed to
// create a payment for that table and return it. It can also be used to perform
// gouging checks before paying for the table.
type PriceTablePaymentFunc func(pt HostPriceTable) (PaymentMethod, error)

// An RPCError may be sent instead of a response object to any RPC.
type RPCError struct {
	Type        types.Specifier
	Data        []byte // structure depends on Type
	Description string // human-readable error string
}

// Error implements the error interface.
func (e *RPCError) Error() string {
	return e.Description
}

// Is reports whether this error matches target.
func (e *RPCError) Is(target error) bool {
	return strings.Contains(e.Description, target.Error())
}

// helper type for encoding and decoding RPC response messages, which can
// represent either valid data or an error.
type rpcResponse struct {
	err  *RPCError
	data protocolObject
}

type protocolObject interface {
	types.EncoderTo
	types.DecoderFrom
}

func writeResponse(w io.Writer, resp protocolObject) error {
	var buf bytes.Buffer
	e := types.NewEncoder(&buf)
	e.WritePrefix(0) // placeholder
	(&rpcResponse{nil, resp}).EncodeTo(e)
	e.Flush()
	b := buf.Bytes()
	binary.LittleEndian.PutUint64(b[:8], uint64(len(b)-8))
	_, err := w.Write(b)
	return err
}

func readResponse(r io.Reader, resp protocolObject) error {
	d := types.NewDecoder(io.LimitedReader{R: r, N: 1 << 20})
	d.ReadPrefix() // ignored
	rr := rpcResponse{nil, resp}
	rr.DecodeFrom(d)
	if d.Err() != nil {
		return d.Err()
	} else if rr.err != nil {
		return rr.err
	}
	return nil
}

func writeRequest(w io.Writer, id types.Specifier, req protocolObject) error {
	if err := writeResponse(w, &id); err != nil {
		return err
	}
	if req != nil {
		return writeResponse(w, req)
	}
	return nil
}

func processPayment(rw io.ReadWriter, payment PaymentMethod) error {
	if err := writeResponse(rw, paymentType(payment)); err != nil {
		return err
	} else if err := writeResponse(rw, payment); err != nil {
		return err
	}
	if pbcr, ok := payment.(*PayByContractRequest); ok {
		var pr paymentResponse
		if err := readResponse(rw, &pr); err != nil {
			return err
		}
		pbcr.HostSignature = pr.Signature
	}
	return nil
}

// A Transport facilitates the exchange of RPCs via the renter-host protocol,
// version 3.
type Transport struct {
	mux *mux.Mux
}

// stream wraps the mux.Stream type to catch the lazily written subscriber
// response the host is sending us before the first Read.
type stream struct {
	*mux.Stream
	once                      sync.Once
	readSubscriberResponseErr error
}

func (s *stream) readSubscriberResponse() {
	// Read response.
	buf := make([]byte, 16)
	_, s.readSubscriberResponseErr = io.ReadFull(s.Stream, buf)
	if s.readSubscriberResponseErr != nil {
		return
	}
	errLen := binary.LittleEndian.Uint64(buf[8:16])
	if errLen == 0 {
		return
	}
	// Read error.
	buf = make([]byte, errLen)
	_, s.readSubscriberResponseErr = io.ReadFull(s.Stream, buf)
	if s.readSubscriberResponseErr != nil {
		return
	}
	s.readSubscriberResponseErr = errors.New(string(buf))
}

// Read passes the read on to the underlying stream. The first time it is called
// it will first try to read the subscriber response.
func (s *stream) Read(b []byte) (int, error) {
	s.once.Do(s.readSubscriberResponse)
	if s.readSubscriberResponseErr != nil {
		return 0, s.readSubscriberResponseErr
	}
	return s.Stream.Read(b)
}

// Write passes the write on to the underlying stream.
func (s *stream) Write(b []byte) (int, error) { return s.Stream.Write(b) }

// DialStream opens a new stream with the host.
func (t *Transport) DialStream() *stream {
	buf := make([]byte, 8+8+len("host"))
	binary.LittleEndian.PutUint64(buf[8:], uint64(len(buf[16:])))
	binary.LittleEndian.PutUint64(buf[:8], uint64(len(buf[8:])))
	copy(buf[16:], "host")

	s := t.mux.DialStream()

	// Write subscriber.
	s.Write(buf)

	return &stream{
		Stream: s,
	}
}

// performSeedHandshake performs the initial seed handshake that the siamux
// expects from the first established stream of a mux.
func (t *Transport) performSeedHandshake() error {
	seed := frand.Uint64n(math.MaxUint64)

	s := t.mux.DialStream()
	defer s.Close()

	// Write seed.
	buf := make([]byte, 8+8)
	binary.LittleEndian.PutUint64(buf[:8], 8)
	binary.LittleEndian.PutUint64(buf[8:], seed)
	if _, err := s.Write(buf); err != nil {
		return err
	}
	// Read seed.
	_, err := io.ReadFull(s, buf)
	return err
}

// Close closes the protocol connection.
func (t *Transport) Close() error {
	return t.mux.Close()
}

// NewRenterTransport establishes a new RHPv3 session over the supplied connection.
func NewRenterTransport(conn net.Conn, hostKey types.PublicKey) (*Transport, error) {
	m, err := mux.Dial(conn, hostKey[:])
	if err != nil {
		return nil, err
	}
	t := &Transport{
		mux: m,
	}
	return t, t.performSeedHandshake()
}

// RPCPriceTable calls the UpdatePriceTable RPC.
func RPCPriceTable(t *Transport, paymentFunc PriceTablePaymentFunc) (pt HostPriceTable, err error) {
	defer wrapErr(&err, "PriceTable")
	s := t.DialStream()
	defer s.Close()

	var ptr rpcUpdatePriceTableResponse
	if err := writeRequest(s, rpcUpdatePriceTableID, nil); err != nil {
		return HostPriceTable{}, err
	} else if err := readResponse(s, &ptr); err != nil {
		return HostPriceTable{}, err
	} else if err := json.Unmarshal(ptr.PriceTableJSON, &pt); err != nil {
		return HostPriceTable{}, err
	} else if payment, err := paymentFunc(pt); err != nil {
		return HostPriceTable{}, err
	} else if err := processPayment(s, payment); err != nil {
		return HostPriceTable{}, err
	} else if err := readResponse(s, &rpcPriceTableResponse{}); err != nil {
		return HostPriceTable{}, err
	}
	return pt, nil
}

// RPCAccountBalance calls the AccountBalance RPC.
func RPCAccountBalance(t *Transport, account Account, price, collateral types.Currency) (bal types.Currency, err error) {
	defer wrapErr(&err, "AccountBalance")
	s := t.DialStream()
	defer s.Close()

	if err := writeRequest(s, rpcAccountBalanceID, &account); err != nil {
		return types.ZeroCurrency, err
	} else if err := readResponse(s, &bal); err != nil {
		return types.ZeroCurrency, err
	}
	return
}

// RPCFundAccount calls the FundAccount RPC.
func RPCFundAccount(t *Transport, payment PaymentMethod, account Account, settingsID SettingsID) (err error) {
	defer wrapErr(&err, "FundAccount")
	s := t.DialStream()
	defer s.Close()

	req := rpcFundAccountRequest{
		Account: account,
	}
	var resp rpcFundAccountResponse
	if err := writeRequest(s, rpcFundAccountID, &settingsID); err != nil {
		return err
	} else if err := writeResponse(s, &req); err != nil {
		return err
	} else if err := processPayment(s, payment); err != nil {
		return err
	} else if err := readResponse(s, &resp); err != nil {
		return err
	}
	return nil
}

// RPCReadRegistry calls the ExecuteProgram RPC with an MDM program that reads
// the specified registry value.
func RPCReadRegistry(t *Transport, payment PaymentMethod, key RegistryKey) (rv RegistryValue, err error) {
	defer wrapErr(&err, "ReadRegistry")
	s := t.DialStream()
	defer s.Close()

	req := &rpcExecuteProgramRequest{
		FileContractID: types.FileContractID{},
		Program: []instruction{{
			Specifier: types.NewSpecifier("ReadRegistry"),
			Args:      encoding.MarshalAll(0, 32),
		}},
		ProgramData: encoding.MarshalAll(key.PublicKey, key.Tweak),
	}
	if _, err := s.Write(rpcExecuteProgramID[:]); err != nil {
		return RegistryValue{}, err
	} else if err := processPayment(s, payment); err != nil {
		return RegistryValue{}, err
	} else if err := writeResponse(s, req); err != nil {
		return RegistryValue{}, err
	}

	var cancellationToken types.Specifier
	readResponse(s, &cancellationToken) // unused

	var resp rpcExecuteProgramResponse
	if err := readResponse(s, &resp); err != nil {
		return RegistryValue{}, err
	} else if resp.OutputLength < 64+8+1 {
		return RegistryValue{}, errors.New("invalid output length")
	}
	buf := make([]byte, resp.OutputLength)
	if _, err := s.Read(buf); err != nil {
		return RegistryValue{}, err
	}
	var sig types.Signature
	copy(sig[:], buf[:64])
	rev := binary.BigEndian.Uint64(buf[64:72])
	data := buf[72 : len(buf)-1]
	typ := buf[len(buf)-1]
	return RegistryValue{
		Data:      data,
		Revision:  rev,
		Type:      typ,
		Signature: sig,
	}, nil
}

// RPCUpdateRegistry calls the ExecuteProgram RPC with an MDM program that
// updates the specified registry value.
func RPCUpdateRegistry(t *Transport, payment PaymentMethod, key RegistryKey, value RegistryValue) (err error) {
	defer wrapErr(&err, "UpdateRegistry")
	s := t.DialStream()
	defer s.Close()

	req := &rpcExecuteProgramRequest{
		FileContractID: types.FileContractID{},
		Program: []instruction{{
			Specifier: types.NewSpecifier("UpdateRegistry"),
			Args:      encoding.Marshal(0),
		}},
		ProgramData: append(encoding.MarshalAll(key.Tweak, value.Revision, value.Signature, key.PublicKey), value.Data...),
	}
	if _, err := s.Write(rpcExecuteProgramID[:]); err != nil {
		return err
	} else if err := processPayment(s, payment); err != nil {
		return err
	} else if err := writeResponse(s, req); err != nil {
		return err
	}

	var cancellationToken types.Specifier
	readResponse(s, &cancellationToken) // unused

	var resp rpcExecuteProgramResponse
	if err := readResponse(s, &resp); err != nil {
		return err
	} else if resp.OutputLength != 0 {
		return errors.New("invalid output length")
	}
	return nil
}
