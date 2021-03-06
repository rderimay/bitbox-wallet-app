// Copyright 2018 Shift Devices AG
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package client implements an Electrum JSON RPC client.
// See https://github.com/kyuupichan/electrumx/blob/159db3f8e70b2b2cbb8e8cd01d1e9df3fe83828f/docs/PROTOCOL.rst
package client

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/digitalbitbox/bitbox-wallet-app/backend/coins/btc/blockchain"
	"github.com/digitalbitbox/bitbox-wallet-app/util/errp"
	"github.com/digitalbitbox/bitbox-wallet-app/util/jsonrpc"
	"github.com/digitalbitbox/bitbox02-api-go/util/semver"
	"github.com/sirupsen/logrus"
)

const (
	clientVersion         = "0.0.1"
	clientProtocolVersion = "1.2"
)

// ElectrumClient is a high level API access to an ElectrumX server.
// See https://github.com/kyuupichan/electrumx/blob/159db3f8e70b2b2cbb8e8cd01d1e9df3fe83828f/docs/PROTOCOL.rst.
type ElectrumClient struct {
	rpc *jsonrpc.RPCClient

	scriptHashNotificationCallbacks     map[string][]func(string)
	scriptHashNotificationCallbacksLock sync.RWMutex

	serverVersion *ServerVersion

	close bool
	log   *logrus.Entry
}

// NewElectrumClient creates a new Electrum client.
func NewElectrumClient(rpcClient *jsonrpc.RPCClient, log *logrus.Entry) *ElectrumClient {
	electrumClient := &ElectrumClient{
		rpc:                             rpcClient,
		scriptHashNotificationCallbacks: map[string][]func(string){},
		log:                             log.WithField("group", "client"),
	}
	// Install a callback for the scripthash notifications, which directs the response to callbacks
	// installed by ScriptHashSubscribe().
	rpcClient.SubscribeNotifications(
		"blockchain.scripthash.subscribe",
		func(responseBytes []byte) {
			// TODO example responsebytes, use for unit testing:
			// "[\"mn31QqyuBum6PFS7VFyo8oUL8Yc8G8MHZA\", \"3b98a4b9bed1312f4f53a1c6c9276b0ad8be68c57a5bcbe651688e4f4191b521\"]"
			response := []string{}
			if err := json.Unmarshal(responseBytes, &response); err != nil {
				electrumClient.log.WithError(err).Error("Failed to unmarshal JSON response")
				return
			}
			if len(response) != 2 {
				electrumClient.log.WithField("response-length", len(response)).Error("Unexpected response (expected 2)")
				return
			}
			scriptHash := response[0]
			status := response[1]
			electrumClient.scriptHashNotificationCallbacksLock.RLock()
			callbacks := electrumClient.scriptHashNotificationCallbacks[scriptHash]
			electrumClient.scriptHashNotificationCallbacksLock.RUnlock()
			for _, callback := range callbacks {
				callback(status)
			}
		},
	)

	rpcClient.OnConnect(func() error {
		// Sends the version and must be the first message, to establish which methods the server
		// accepts.
		version, err := electrumClient.ServerVersion()
		if err != nil {
			return err
		}
		electrumClient.serverVersion = version
		log.WithField("server-version", version).Debug("electrumx server version")
		return nil
	})
	rpcClient.RegisterHeartbeat("server.ping")

	return electrumClient
}

// ConnectionStatus returns the current connection status of the backend.
func (client *ElectrumClient) ConnectionStatus() blockchain.Status {
	switch client.rpc.ConnectionStatus() {
	case jsonrpc.CONNECTED:
		return blockchain.CONNECTED
	case jsonrpc.DISCONNECTED:
		return blockchain.DISCONNECTED
	}
	panic(errp.New("Connection status could not be determined"))
}

// RegisterOnConnectionStatusChangedEvent registers an event that forwards the connection status from
// the underlying client to the given callback.
func (client *ElectrumClient) RegisterOnConnectionStatusChangedEvent(onConnectionStatusChanged func(blockchain.Status)) {
	client.rpc.RegisterOnConnectionStatusChangedEvent(func(status jsonrpc.Status) {
		switch status {
		case jsonrpc.CONNECTED:
			onConnectionStatusChanged(blockchain.CONNECTED)
		case jsonrpc.DISCONNECTED:
			onConnectionStatusChanged(blockchain.DISCONNECTED)
		}
	})
}

// ServerVersion is returned by ServerVersion().
type ServerVersion struct {
	Version         string
	ProtocolVersion *semver.SemVer
}

func (version *ServerVersion) String() string {
	return fmt.Sprintf("%s;%s", version.Version, version.ProtocolVersion)
}

// UnmarshalJSON implements the json.Unmarshaler interface.
func (version *ServerVersion) UnmarshalJSON(b []byte) error {
	slice := []string{}
	if err := json.Unmarshal(b, &slice); err != nil {
		return errp.WithContext(errp.Wrap(err, "Failed to unmarshal JSON"), errp.Context{"raw": string(b)})
	}
	if len(slice) != 2 {
		return errp.WithContext(errp.New("Unexpected reply"), errp.Context{"raw": string(b)})
	}
	version.Version = slice[0]
	protocolVersion := slice[1]
	// We expect the protocolVersion to be either major.minor or major.minor.patch.
	protocolSemVer, err := semver.NewSemVerFromString(protocolVersion)
	if err != nil {
		protocolSemVer, err = semver.NewSemVerFromString(protocolVersion + ".0")
		if err != nil {
			return err
		}
	}
	version.ProtocolVersion = protocolSemVer
	return nil
}

// ServerVersion does the server.version() RPC call.
// https://github.com/kyuupichan/electrumx/blob/159db3f8e70b2b2cbb8e8cd01d1e9df3fe83828f/docs/PROTOCOL.rst#serverversion
func (client *ElectrumClient) ServerVersion() (*ServerVersion, error) {
	response := &ServerVersion{}
	err := client.rpc.MethodSync(response, "server.version", clientVersion, clientProtocolVersion)
	return response, err
}

// ServerFeatures is returned by ServerFeatures().
type ServerFeatures struct {
	GenesisHash string `json:"genesis_hash"`
}

// ServerFeatures does the server.features() RPC call.
// https://github.com/kyuupichan/electrumx/blob/159db3f8e70b2b2cbb8e8cd01d1e9df3fe83828f/docs/PROTOCOL.rst#serverfeatures
func (client *ElectrumClient) ServerFeatures() (*ServerFeatures, error) {
	response := &ServerFeatures{}
	err := client.rpc.MethodSync(response, "server.features")
	return response, err
}

// Balance is returned by ScriptHashGetBalance().
type Balance struct {
	Confirmed   int64 `json:"confirmed"`
	Unconfirmed int64 `json:"unconfirmed"`
}

// ScriptHashGetBalance does the blockchain.scripthash.get_balance() RPC call.
// https://github.com/kyuupichan/electrumx/blob/159db3f8e70b2b2cbb8e8cd01d1e9df3fe83828f/docs/PROTOCOL.rst#blockchainscripthashget_balance
func (client *ElectrumClient) ScriptHashGetBalance(
	scriptHashHex string,
	success func(*Balance) error,
	cleanup func(error)) {
	client.rpc.Method(
		func(responseBytes []byte) error {
			response := &Balance{}
			if err := json.Unmarshal(responseBytes, response); err != nil {
				client.log.WithError(err).Error("Failed to unmarshal JSON response")
				return errp.WithStack(err)
			}
			return success(response)
		},
		func() func(error) {
			return cleanup
		},
		"blockchain.scripthash.get_balance",
		scriptHashHex)
}

// ScriptHashGetHistory does the blockchain.scripthash.get_history() RPC call.
// https://github.com/kyuupichan/electrumx/blob/159db3f8e70b2b2cbb8e8cd01d1e9df3fe83828f/docs/PROTOCOL.rst#blockchainscripthashget_history
func (client *ElectrumClient) ScriptHashGetHistory(
	scriptHashHex blockchain.ScriptHashHex,
	success func(blockchain.TxHistory),
	cleanup func(error),
) {
	client.rpc.Method(
		func(responseBytes []byte) error {
			txs := blockchain.TxHistory{}
			if err := json.Unmarshal(responseBytes, &txs); err != nil {
				client.log.WithError(err).Error("Failed to unmarshal JSON response")
				return errp.WithStack(err)
			}
			success(txs)
			return nil
		},
		func() func(error) {
			return cleanup
		},
		"blockchain.scripthash.get_history",
		string(scriptHashHex))
}

// ScriptHashSubscribe does the blockchain.scripthash.subscribe() RPC call.
// https://github.com/kyuupichan/electrumx/blob/159db3f8e70b2b2cbb8e8cd01d1e9df3fe83828f/docs/PROTOCOL.rst#blockchainscripthashsubscribe
func (client *ElectrumClient) ScriptHashSubscribe(
	setupAndTeardown func() func(error),
	scriptHashHex blockchain.ScriptHashHex,
	success func(string),
) {
	client.scriptHashNotificationCallbacksLock.Lock()
	if _, ok := client.scriptHashNotificationCallbacks[string(scriptHashHex)]; !ok {
		client.scriptHashNotificationCallbacks[string(scriptHashHex)] = []func(string){}
	}
	client.scriptHashNotificationCallbacks[string(scriptHashHex)] = append(
		client.scriptHashNotificationCallbacks[string(scriptHashHex)],
		success,
	)
	client.scriptHashNotificationCallbacksLock.Unlock()
	client.rpc.Method(
		func(responseBytes []byte) error {
			var response *string
			if err := json.Unmarshal(responseBytes, &response); err != nil {
				client.log.WithError(err).Error("Failed to unmarshal JSON response")
				return errp.WithStack(err)
			}
			if response == nil {
				success("")
			} else {
				success(*response)
			}
			return nil
		},
		setupAndTeardown,
		"blockchain.scripthash.subscribe",
		string(scriptHashHex))
}

func parseTX(rawTXHex string) (*wire.MsgTx, error) {
	rawTX, err := hex.DecodeString(rawTXHex)
	if err != nil {
		return nil, errp.Wrap(err, "Failed to decode transaction hex")
	}
	tx := &wire.MsgTx{}
	if err := tx.BtcDecode(bytes.NewReader(rawTX), 0, wire.WitnessEncoding); err != nil {
		return nil, errp.Wrap(err, "Failed to decode BTC transaction")
	}
	return tx, nil
}

// TransactionGet downloads a transaction.
// See https://github.com/kyuupichan/electrumx/blob/159db3f8e70b2b2cbb8e8cd01d1e9df3fe83828f/docs/PROTOCOL.rst#blockchaintransactionget
func (client *ElectrumClient) TransactionGet(
	txHash chainhash.Hash,
	success func(*wire.MsgTx),
	cleanup func(error),
) {
	client.rpc.Method(
		func(responseBytes []byte) error {
			var rawTXHex string
			if err := json.Unmarshal(responseBytes, &rawTXHex); err != nil {
				return errp.WithStack(err)
			}
			tx, err := parseTX(rawTXHex)
			if err != nil {
				return err
			}
			success(tx)
			return nil
		},
		func() func(error) {
			return cleanup
		},
		"blockchain.transaction.get",
		txHash.String())
}

type electrumHeader struct {
	// Provided by v1.4
	BlockHeight int `json:"block_height"`
	// Provided by v1.2
	Height int `json:"height"`
}

func (h *electrumHeader) height(serverVersion *semver.SemVer) int {
	if serverVersion.AtLeast(semver.NewSemVer(1, 4, 0)) {
		return h.Height
	}
	return h.BlockHeight
}

// HeadersSubscribe does the blockchain.headers.subscribe() RPC call.
// https://github.com/kyuupichan/electrumx/blob/159db3f8e70b2b2cbb8e8cd01d1e9df3fe83828f/docs/PROTOCOL.rst#blockchainheaderssubscribe
func (client *ElectrumClient) HeadersSubscribe(
	setupAndTeardown func() func(error),
	success func(*blockchain.Header),
) {
	client.rpc.SubscribeNotifications("blockchain.headers.subscribe", func(responseBytes []byte) {
		response := []json.RawMessage{}
		if err := json.Unmarshal(responseBytes, &response); err != nil {
			client.log.WithError(err).Error("could not handle header notification")
			return
		}
		if len(response) != 1 {
			client.log.Error("could not handle header notification")
			return
		}
		header := &electrumHeader{}
		if err := json.Unmarshal(response[0], header); err != nil {
			client.log.WithError(err).Error("could not handle header notification")
			return
		}
		success(&blockchain.Header{BlockHeight: header.height(client.serverVersion.ProtocolVersion)})
	})
	client.rpc.Method(
		func(responseBytes []byte) error {
			header := &electrumHeader{}
			if err := json.Unmarshal(responseBytes, header); err != nil {
				return errp.WithStack(err)
			}
			success(&blockchain.Header{BlockHeight: header.height(client.serverVersion.ProtocolVersion)})
			return nil
		},
		setupAndTeardown,
		"blockchain.headers.subscribe")
}

// TXHash wraps chainhash.Hash for json deserialization.
type TXHash chainhash.Hash

// Hash returns the wrapped hash.
func (txHash *TXHash) Hash() chainhash.Hash {
	return chainhash.Hash(*txHash)
}

// MarshalJSON implements the json.Marshaler interface.
func (txHash *TXHash) MarshalJSON() ([]byte, error) {
	return json.Marshal(txHash.Hash().String())
}

// UnmarshalJSON implements the json.Unmarshaler interface.
func (txHash *TXHash) UnmarshalJSON(jsonBytes []byte) error {
	var txHashStr string
	if err := json.Unmarshal(jsonBytes, &txHashStr); err != nil {
		return errp.WithStack(err)
	}
	t, err := chainhash.NewHashFromStr(txHashStr)
	if err != nil {
		return errp.WithStack(err)
	}
	*txHash = TXHash(*t)
	return nil
}

// UTXO is the data returned by the listunspent RPC call.
type UTXO struct {
	TXPos  int    `json:"tx_pos"`
	Value  int64  `json:"value"`
	TXHash string `json:"tx_hash"`
	Height int    `json:"height"`
}

// ScriptHashListUnspent does the blockchain.address.listunspent() RPC call.
// https://github.com/kyuupichan/electrumx/blob/159db3f8e70b2b2cbb8e8cd01d1e9df3fe83828f/docs/PROTOCOL.rst#blockchainscripthashlistunspent
func (client *ElectrumClient) ScriptHashListUnspent(scriptHashHex string) ([]*UTXO, error) {
	response := []*UTXO{}
	if err := client.rpc.MethodSync(&response, "blockchain.scripthash.listunspent", scriptHashHex); err != nil {
		return nil, errp.WithStack(err)
	}
	return response, nil
}

// TransactionBroadcast does the blockchain.transaction.broadcast() RPC call.
// https://github.com/kyuupichan/electrumx/blob/159db3f8e70b2b2cbb8e8cd01d1e9df3fe83828f/docs/PROTOCOL.rst#blockchaintransactionbroadcast
func (client *ElectrumClient) TransactionBroadcast(transaction *wire.MsgTx) error {
	rawTx := &bytes.Buffer{}
	_ = transaction.BtcEncode(rawTx, 0, wire.WitnessEncoding)
	rawTxHex := hex.EncodeToString(rawTx.Bytes())
	var response string
	if err := client.rpc.MethodSync(&response, "blockchain.transaction.broadcast", rawTxHex); err != nil {
		return errp.Wrap(err, "Failed to broadcast transaction")
	}
	// TxHash() deviates from the hash of rawTxHex in case of a segwit tx. The stripped transaction
	// ID is used.
	if response != transaction.TxHash().String() {
		return errp.WithContext(errp.New("Response is unexpected (expected TX hash)"),
			errp.Context{"response": response})
	}
	return nil
}

// RelayFee does the blockchain.relayfee() RPC call.
// https://github.com/kyuupichan/electrumx/blob/159db3f8e70b2b2cbb8e8cd01d1e9df3fe83828f/docs/PROTOCOL.rst#blockchainrelayfee
func (client *ElectrumClient) RelayFee(
	success func(btcutil.Amount),
	cleanup func(error),
) {
	client.rpc.Method(func(responseBytes []byte) error {
		var fee float64
		if err := json.Unmarshal(responseBytes, &fee); err != nil {
			return errp.Wrap(err, "Failed to unmarshal JSON")
		}
		amount, err := btcutil.NewAmount(fee)
		if err != nil {
			return errp.Wrap(err, "Failed to construct BTC amount")
		}
		success(amount)
		return nil
	}, func() func(error) { return cleanup }, "blockchain.relayfee")
}

// EstimateFee estimates the fee rate (unit/kB) needed to be confirmed within the given number of
// blocks. If the fee rate could not be estimated by the blockchain node, `nil` is passed to the
// success callback.
// https://github.com/kyuupichan/electrumx/blob/159db3f8e70b2b2cbb8e8cd01d1e9df3fe83828f/docs/PROTOCOL.rst#blockchainestimatefee
func (client *ElectrumClient) EstimateFee(
	number int,
	success func(*btcutil.Amount),
	cleanup func(error),
) {
	client.rpc.Method(
		func(responseBytes []byte) error {
			var fee float64
			if err := json.Unmarshal(responseBytes, &fee); err != nil {
				return errp.Wrap(err, "Failed to unmarshal JSON")
			}
			if fee == -1 {
				success(nil)
				return nil
			}
			amount, err := btcutil.NewAmount(fee)
			if err != nil {
				return errp.Wrap(err, "Failed to construct BTC amount")
			}
			success(&amount)
			return nil
		},
		func() func(error) {
			return cleanup
		},
		"blockchain.estimatefee",
		number)
}

func parseHeaders(reader io.Reader) ([]*wire.BlockHeader, error) {
	headers := []*wire.BlockHeader{}
	for {
		header := &wire.BlockHeader{}
		err := header.BtcDecode(reader, 0, wire.WitnessEncoding)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		headers = append(headers, header)
	}
	return headers, nil
}

// Headers does the blockchain.block.headers() RPC call. See
// https://github.com/kyuupichan/electrumx/blob/1.3/docs/protocol-methods.rst#blockchainblockheaders
func (client *ElectrumClient) Headers(
	startHeight int, count int,
	success func(headers []*wire.BlockHeader, max int),
) {
	client.rpc.Method(
		func(responseBytes []byte) error {
			var response struct {
				Hex   string `json:"hex"`
				Count int    `json:"count"`
				Max   int    `json:"max"`
			}
			if err := json.Unmarshal(responseBytes, &response); err != nil {
				return errp.WithStack(err)
			}
			headers, err := parseHeaders(hex.NewDecoder(strings.NewReader(response.Hex)))
			if err != nil {
				return err
			}
			if len(headers) != response.Count {
				return errp.Newf(
					"unexpected electrumx reply: should have gotten %d headers, but got %d",
					response.Count,
					len(headers))
			}
			success(headers, response.Max)
			return nil
		},
		func() func(error) {
			return func(error) {}
		},
		"blockchain.block.headers",
		startHeight, count)
}

// GetMerkle does the blockchain.transaction.get_merkle() RPC call. See
// https://github.com/kyuupichan/electrumx/blob/1.3/docs/protocol-methods.rst#blockchaintransactionget_merkle
func (client *ElectrumClient) GetMerkle(
	txHash chainhash.Hash, height int,
	success func(merkle []blockchain.TXHash, pos int),
	cleanup func(error),
) {
	client.rpc.Method(
		func(responseBytes []byte) error {
			var response struct {
				Merkle      []blockchain.TXHash `json:"merkle"`
				Pos         int                 `json:"pos"`
				BlockHeight int                 `json:"block_height"`
			}
			if err := json.Unmarshal(responseBytes, &response); err != nil {
				return errp.WithStack(err)
			}
			if response.BlockHeight != height {
				return errp.Newf("height should be %d, but got %d", height, response.BlockHeight)
			}
			success(response.Merkle, response.Pos)
			return nil
		},
		func() func(error) {
			return cleanup
		},
		"blockchain.transaction.get_merkle",
		txHash.String(), height)
}

// Close closes the connection.
func (client *ElectrumClient) Close() {
	client.close = true
	client.rpc.Close()
}
