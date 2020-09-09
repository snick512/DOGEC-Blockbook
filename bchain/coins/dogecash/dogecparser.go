package dogec

import (
	"blockbook/bchain"
	"blockbook/bchain/coins/btc"
	"blockbook/bchain/coins/utils"
	"bytes"
	"fmt"
	"io"

	"encoding/binary"
	"encoding/hex"
	"encoding/json"

	"math"
	"math/big"

	"github.com/golang/glog"
	"github.com/juju/errors"
	"github.com/martinboehm/btcd/blockchain"
	"github.com/martinboehm/btcd/wire"
	"github.com/martinboehm/btcutil/chaincfg"
)

const (
	// Net Magics
	MainnetMagic wire.BitcoinNet = 0xe9fdc490
	TestnetMagic wire.BitcoinNet = 0xba657645

	// Zerocoin op codes
	OP_ZEROCOINMINT  = 0xc1
	OP_ZEROCOINSPEND = 0xc2

	// Labels
	ZCMINT_LABEL  = "Zerocoin Mint"
	ZCSPEND_LABEL = "Zerocoin Spend"
	CBASE_LABEL   = "CoinBase TX"
	CSTAKE_LABEL  = "CoinStake TX"

	// Dummy Internal Addresses
	CBASE_ADDR_INT  = 0xf7
	CSTAKE_ADDR_INT = 0xf8

	// Number of blocks per budget cycle
	nBlocksPerPeriod = 43200
)

var (
	MainNetParams chaincfg.Params
	TestNetParams chaincfg.Params
)

func init() {
	// DogeCash mainnet Address encoding magics
	MainNetParams = chaincfg.MainNetParams
	MainNetParams.Net = MainnetMagic
	MainNetParams.PubKeyHashAddrID = []byte{30} // starting with 'D'
	MainNetParams.ScriptHashAddrID = []byte{19}
	MainNetParams.PrivateKeyID = []byte{122}

	// DogeCash testnet Address encoding magics
	TestNetParams = chaincfg.TestNet3Params
	TestNetParams.Net = TestnetMagic
	TestNetParams.PubKeyHashAddrID = []byte{139} // starting with 'x' or 'y'
	TestNetParams.ScriptHashAddrID = []byte{19}
	TestNetParams.PrivateKeyID = []byte{239}
}

// DogeCParser handle
type DogeCParser struct {
	*btc.BitcoinParser
	baseparser                         *bchain.BaseParser
	BitcoinOutputScriptToAddressesFunc btc.OutputScriptToAddressesFunc
}

// NewDogeCParser returns new DogeCParser instance
func NewDogeCParser(params *chaincfg.Params, c *btc.Configuration) *DogeCParser {
	p := &DogeCParser{
		BitcoinParser: btc.NewBitcoinParser(params, c),
		baseparser:    &bchain.BaseParser{},
	}
	p.BitcoinOutputScriptToAddressesFunc = p.OutputScriptToAddressesFunc
	p.OutputScriptToAddressesFunc = p.outputScriptToAddresses
	return p
}

// GetChainParams contains network parameters for the main DogeC network
func GetChainParams(chain string) *chaincfg.Params {
	if !chaincfg.IsRegistered(&MainNetParams) {
		err := chaincfg.Register(&MainNetParams)
		if err == nil {
			err = chaincfg.Register(&TestNetParams)
		}
		if err != nil {
			panic(err)
		}
	}
	switch chain {
	case "test":
		return &TestNetParams
	default:
		return &MainNetParams
	}
}

// ParseBlock parses raw block to our Block struct
func (p *DogeCParser) ParseBlock(b []byte) (*bchain.Block, error) {
	r := bytes.NewReader(b)
	w := wire.MsgBlock{}
	h := wire.BlockHeader{}
	err := h.Deserialize(r)
	if err != nil {
		return nil, errors.Annotatef(err, "Deserialize")
	}

	if h.Version > 3 && h.Version < 7 {
		// Skip past AccumulatorCheckpoint which was added in dogec block version 4 and removed in v7
		r.Seek(32, io.SeekCurrent)
	}

	err = utils.DecodeTransactions(r, 0, wire.WitnessEncoding, &w)
	if err != nil {
		return nil, errors.Annotatef(err, "DecodeTransactions")
	}

	txs := make([]bchain.Tx, len(w.Transactions))
	for ti, t := range w.Transactions {
		txs[ti] = p.TxFromMsgTx(t, false)
	}

	return &bchain.Block{
		BlockHeader: bchain.BlockHeader{
			Size: len(b),
			Time: h.Timestamp.Unix(),
		},
		Txs: txs,
	}, nil
}

// PackTx packs transaction to byte array using protobuf
func (p *DogeCParser) PackTx(tx *bchain.Tx, height uint32, blockTime int64) ([]byte, error) {
	return p.baseparser.PackTx(tx, height, blockTime)
}

// UnpackTx unpacks transaction from protobuf byte array
func (p *DogeCParser) UnpackTx(buf []byte) (*bchain.Tx, uint32, error) {
	return p.baseparser.UnpackTx(buf)
}

// ParseTx parses byte array containing transaction and returns Tx struct
func (p *DogeCParser) ParseTx(b []byte) (*bchain.Tx, error) {
	t := wire.MsgTx{}
	r := bytes.NewReader(b)
	if err := t.Deserialize(r); err != nil {
		return nil, err
	}
	tx := p.TxFromMsgTx(&t, true)
	tx.Hex = hex.EncodeToString(b)
	return &tx, nil
}

// Parses tx and adds handling for OP_ZEROCOINSPEND inputs
func (p *DogeCParser) TxFromMsgTx(t *wire.MsgTx, parseAddresses bool) bchain.Tx {
	vin := make([]bchain.Vin, len(t.TxIn))
	for i, in := range t.TxIn {

		// extra check to not confuse Tx with single OP_ZEROCOINSPEND input as a coinbase Tx
		if !isZeroCoinSpendScript(in.SignatureScript) && blockchain.IsCoinBaseTx(t) {
			vin[i] = bchain.Vin{
				Coinbase: hex.EncodeToString(in.SignatureScript),
				Sequence: in.Sequence,
			}
			break
		}

		s := bchain.ScriptSig{
			Hex: hex.EncodeToString(in.SignatureScript),
			// missing: Asm,
		}

		txid := in.PreviousOutPoint.Hash.String()

		vin[i] = bchain.Vin{
			Txid:      txid,
			Vout:      in.PreviousOutPoint.Index,
			Sequence:  in.Sequence,
			ScriptSig: s,
		}
	}
	vout := make([]bchain.Vout, len(t.TxOut))
	for i, out := range t.TxOut {
		addrs := []string{}
		if parseAddresses {
			addrs, _, _ = p.OutputScriptToAddressesFunc(out.PkScript)
		}
		s := bchain.ScriptPubKey{
			Hex:       hex.EncodeToString(out.PkScript),
			Addresses: addrs,
			// missing: Asm,
			// missing: Type,
		}
		if s.Hex == "" {
			if blockchain.IsCoinBaseTx(t) && !isZeroCoinSpendScript(t.TxIn[0].SignatureScript) {
				s.Hex = fmt.Sprintf("%02x", CBASE_ADDR_INT)
			} else {
				s.Hex = fmt.Sprintf("%02x", CSTAKE_ADDR_INT)
			}
		}
		var vs big.Int
		vs.SetInt64(out.Value)
		vout[i] = bchain.Vout{
			ValueSat:     vs,
			N:            uint32(i),
			ScriptPubKey: s,
		}
	}
	tx := bchain.Tx{
		Txid:     t.TxHash().String(),
		Version:  t.Version,
		LockTime: t.LockTime,
		Vin:      vin,
		Vout:     vout,
		// skip: BlockHash,
		// skip: Confirmations,
		// skip: Time,
		// skip: Blocktime,
	}
	return tx
}

// ParseTxFromJson parses JSON message containing transaction and returns Tx struct
func (p *DogeCParser) ParseTxFromJson(msg json.RawMessage) (*bchain.Tx, error) {
	var tx bchain.Tx
	err := json.Unmarshal(msg, &tx)
	if err != nil {
		return nil, err
	}

	for i := range tx.Vout {
		vout := &tx.Vout[i]
		// convert vout.JsonValue to big.Int and clear it, it is only temporary value used for unmarshal
		vout.ValueSat, err = p.AmountToBigInt(vout.JsonValue)
		if err != nil {
			return nil, err
		}
		vout.JsonValue = ""

		if vout.ScriptPubKey.Addresses == nil {
			vout.ScriptPubKey.Addresses = []string{}
		}

		if vout.ScriptPubKey.Hex == "" {
			if isCoinbaseTx(tx) {
				vout.ScriptPubKey.Hex = fmt.Sprintf("%02x", CBASE_ADDR_INT)
			} else {
				vout.ScriptPubKey.Hex = fmt.Sprintf("%02x", CSTAKE_ADDR_INT)
			}
		}

	}
	return &tx, nil
}

// outputScriptToAddresses converts ScriptPubKey to bitcoin addresses
func (p *DogeCParser) outputScriptToAddresses(script []byte) ([]string, bool, error) {
	if isZeroCoinSpendScript(script) {
		return []string{ZCSPEND_LABEL}, false, nil
	}
	if isZeroCoinMintScript(script) {
		return []string{ZCMINT_LABEL}, false, nil
	}
	if isCoinBaseFakeAddr(script) {
		return []string{CBASE_LABEL}, false, nil
	}
	if isCoinStakeFakeAddr(script) {
		return []string{CSTAKE_LABEL}, false, nil
	}

	rv, s, _ := p.BitcoinOutputScriptToAddressesFunc(script)
	return rv, s, nil
}

func (p *DogeCParser) GetAddrDescForUnknownInput(tx *bchain.Tx, input int) bchain.AddressDescriptor {
	if len(tx.Vin) > input {
		scriptHex := tx.Vin[input].ScriptSig.Hex

		if scriptHex != "" {
			script, _ := hex.DecodeString(scriptHex)
			return script
		}
	}

	s := make([]byte, 10)
	return s
}

func (p *DogeCParser) GetValueSatForUnknownInput(tx *bchain.Tx, input int) *big.Int {
	if len(tx.Vin) > input {
		scriptHex := tx.Vin[input].ScriptSig.Hex
		if scriptHex != "" {
			script, _ := hex.DecodeString(scriptHex)
			if isZeroCoinSpendScript(script) {
				valueSat, err := p.GetValueSatFromZerocoinSpend(script)
				if err != nil {
					glog.Warningf("tx %v: input %d unable to convert denom to big int", tx.Txid, input)
					return big.NewInt(0)
				}
				return valueSat
			}
		}
	}
	return big.NewInt(0)
}

// Decodes the amount from the zerocoin spend script
func (p *DogeCParser) GetValueSatFromZerocoinSpend(signatureScript []byte) (*big.Int, error) {
	r := bytes.NewReader(signatureScript)
	r.Seek(1, io.SeekCurrent) // skip opcode
	len, err := Uint8(r)      // get serialized coinspend size
	if err != nil {
		return nil, err
	}
	r.Seek(int64(len), io.SeekCurrent)           // and skip its bytes
	denom, err := Uint32(r, binary.LittleEndian) // get denomination
	if err != nil {
		return nil, err
	}

	return big.NewInt(int64(denom) * 1e8), nil
}

// Checks if script is OP_ZEROCOINMINT
func isZeroCoinMintScript(signatureScript []byte) bool {
	return len(signatureScript) > 1 && signatureScript[0] == OP_ZEROCOINMINT
}

// Checks if script is OP_ZEROCOINSPEND
func isZeroCoinSpendScript(signatureScript []byte) bool {
	return len(signatureScript) >= 100 && signatureScript[0] == OP_ZEROCOINSPEND
}

// Checks if script is dummy internal address for Coinbase
func isCoinBaseFakeAddr(signatureScript []byte) bool {
	return len(signatureScript) == 1 && signatureScript[0] == CBASE_ADDR_INT
}

// Checks if script is dummy internal address for Stake
func isCoinStakeFakeAddr(signatureScript []byte) bool {
	return len(signatureScript) == 1 && signatureScript[0] == CSTAKE_ADDR_INT
}

// Checks if a Tx is coinbase
func isCoinbaseTx(tx bchain.Tx) bool {
	return len(tx.Vin) == 1 && tx.Vin[0].Coinbase != "" && tx.Vin[0].Sequence == math.MaxUint32
}
