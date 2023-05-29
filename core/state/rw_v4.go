package state

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"

	"github.com/holiman/uint256"
	"github.com/ledgerwatch/log/v3"
	btree2 "github.com/tidwall/btree"

	"github.com/ledgerwatch/erigon-lib/commitment"
	"github.com/ledgerwatch/erigon-lib/common"
	"github.com/ledgerwatch/erigon-lib/common/length"
	"github.com/ledgerwatch/erigon-lib/state"
	"github.com/ledgerwatch/erigon/core/types/accounts"
)

type StateWriterV4 struct {
	*state.SharedDomains
}

func WrapStateIO(s *state.SharedDomains) (*StateWriterV4, *StateReaderV4) {
	w, r := &StateWriterV4{s}, &StateReaderV4{s}
	return w, r
}

func (w *StateWriterV4) UpdateAccountData(address common.Address, original, account *accounts.Account) error {
	//fmt.Printf("account [%x]=>{Balance: %d, Nonce: %d, Root: %x, CodeHash: %x} txNum: %d\n", address, &account.Balance, account.Nonce, account.Root, account.CodeHash, w.txNum)
	return w.SharedDomains.UpdateAccountData(address.Bytes(), accounts.SerialiseV3(account), accounts.SerialiseV3(original))
}

func (w *StateWriterV4) UpdateAccountCode(address common.Address, incarnation uint64, codeHash common.Hash, code []byte) error {
	//addressBytes, codeHashBytes := address.Bytes(), codeHash.Bytes()
	//fmt.Printf("code [%x] => [%x] CodeHash: %x, txNum: %d\n", address, code, codeHash, w.txNum)
	return w.SharedDomains.UpdateAccountCode(address.Bytes(), code, nil)
}

func (w *StateWriterV4) DeleteAccount(address common.Address, original *accounts.Account) error {
	addressBytes := address.Bytes()
	return w.SharedDomains.DeleteAccount(addressBytes, accounts.SerialiseV3(original))
}

func (w *StateWriterV4) WriteAccountStorage(address common.Address, incarnation uint64, key *common.Hash, original, value *uint256.Int) error {
	if original.Eq(value) {
		return nil
	}
	//fmt.Printf("storage [%x] [%x] => [%x], txNum: %d\n", address, *key, v, w.txNum)
	return w.SharedDomains.WriteAccountStorage(address.Bytes(), key.Bytes(), value.Bytes(), original.Bytes())
}

func (w *StateWriterV4) CreateContract(address common.Address) error { return nil }
func (w *StateWriterV4) WriteChangeSets() error                      { return nil }
func (w *StateWriterV4) WriteHistory() error                         { return nil }

type StateReaderV4 struct {
	*state.SharedDomains
}

func (s *StateReaderV4) ReadAccountData(address common.Address) (*accounts.Account, error) {
	enc, err := s.LatestAccount(address.Bytes())
	if err != nil {
		return nil, err
	}
	if len(enc) == 0 {
		return nil, nil
	}
	var a accounts.Account
	if err := accounts.DeserialiseV3(&a, enc); err != nil {
		return nil, err
	}
	return &a, nil
}

func (s *StateReaderV4) ReadAccountStorage(address common.Address, incarnation uint64, key *common.Hash) ([]byte, error) {
	enc, err := s.LatestStorage(address.Bytes(), key.Bytes())
	if err != nil {
		return nil, err
	}
	if enc == nil {
		return nil, nil
	}
	if len(enc) == 1 && enc[0] == 0 {
		return nil, nil
	}
	return enc, nil
}

func (s *StateReaderV4) ReadAccountCode(address common.Address, incarnation uint64, codeHash common.Hash) ([]byte, error) {
	return s.LatestCode(address.Bytes())
}

func (s *StateReaderV4) ReadAccountCodeSize(address common.Address, incarnation uint64, codeHash common.Hash) (int, error) {
	c, err := s.ReadAccountCode(address, incarnation, codeHash)
	if err != nil {
		return 0, err
	}
	return len(c), nil
}

func (s *StateReaderV4) ReadAccountIncarnation(address common.Address) (uint64, error) {
	return 0, nil
}

type MultiStateWriter struct {
	writers []StateWriter
}

func NewMultiStateWriter(w ...StateWriter) *MultiStateWriter {
	return &MultiStateWriter{
		writers: w,
	}
}

func (m *MultiStateWriter) UpdateAccountData(address common.Address, original, account *accounts.Account) error {
	for i, w := range m.writers {
		if err := w.UpdateAccountData(address, original, account); err != nil {
			return fmt.Errorf("%T at pos %d: UpdateAccountData: %w", w, i, err)
		}
	}
	return nil
}

func (m *MultiStateWriter) UpdateAccountCode(address common.Address, incarnation uint64, codeHash common.Hash, code []byte) error {
	for i, w := range m.writers {
		if err := w.UpdateAccountCode(address, incarnation, codeHash, code); err != nil {
			return fmt.Errorf("%T at pos %d: UpdateAccountCode: %w", w, i, err)
		}
	}
	return nil
}

func (m *MultiStateWriter) DeleteAccount(address common.Address, original *accounts.Account) error {
	for i, w := range m.writers {
		if err := w.DeleteAccount(address, original); err != nil {
			return fmt.Errorf("%T at pos %d: DeleteAccount: %w", w, i, err)
		}
	}
	return nil
}

func (m *MultiStateWriter) WriteAccountStorage(address common.Address, incarnation uint64, key *common.Hash, original, value *uint256.Int) error {
	for i, w := range m.writers {
		if err := w.WriteAccountStorage(address, incarnation, key, original, value); err != nil {
			return fmt.Errorf("%T at pos %d: WriteAccountStorage: %w", w, i, err)
		}
	}
	return nil
}

func (m *MultiStateWriter) CreateContract(address common.Address) error {
	for i, w := range m.writers {
		if err := w.CreateContract(address); err != nil {
			return fmt.Errorf("%T at pos %d: CreateContract: %w", w, i, err)
		}
	}
	return nil
}

type MultiStateReader struct {
	readers []StateReader
	compare bool // use first read as ethalon value for current read iteration
}

func NewMultiStateReader(compare bool, r ...StateReader) *MultiStateReader {
	return &MultiStateReader{readers: r, compare: compare}
}
func (m *MultiStateReader) ReadAccountData(address common.Address) (*accounts.Account, error) {
	var vo accounts.Account
	var isnil bool
	for i, r := range m.readers {
		v, err := r.ReadAccountData(address)
		if err != nil {
			return nil, err
		}
		if i == 0 {
			if v == nil {
				isnil = true
				continue
			}
			vo = *v
		}

		if !m.compare {
			continue
		}
		if isnil {
			if v != nil {
				log.Warn("state read invalid",
					"reader", fmt.Sprintf("%d %T", i, r), "addr", address.String(),
					"m", "nil expected, got something")

			} else {
				continue
			}
		}
		buf := new(strings.Builder)
		if vo.Nonce != v.Nonce {
			buf.WriteString(fmt.Sprintf("nonce exp: %d, %d", vo.Nonce, v.Nonce))
		}
		if !bytes.Equal(vo.CodeHash[:], v.CodeHash[:]) {
			buf.WriteString(fmt.Sprintf("code exp: %x, %x", vo.CodeHash[:], v.CodeHash[:]))
		}
		if !vo.Balance.Eq(&v.Balance) {
			buf.WriteString(fmt.Sprintf("bal exp: %v, %v", vo.Balance.String(), v.Balance.String()))
		}
		if !bytes.Equal(vo.Root[:], v.Root[:]) {
			buf.WriteString(fmt.Sprintf("root exp: %x, %x", vo.Root[:], v.Root[:]))
		}
		if buf.Len() > 0 {
			log.Warn("state read invalid",
				"reader", fmt.Sprintf("%d %T", i, r), "addr", address.String(),
				"m", buf.String())
		}
	}
	return &vo, nil
}

func (m *MultiStateReader) ReadAccountStorage(address common.Address, incarnation uint64, key *common.Hash) ([]byte, error) {
	var so []byte
	for i, r := range m.readers {
		s, err := r.ReadAccountStorage(address, incarnation, key)
		if err != nil {
			return nil, err
		}
		if i == 0 {
			so = common.Copy(s)
		}
		if !m.compare {
			continue
		}
		if !bytes.Equal(so, s) {
			log.Warn("state storage invalid read",
				"reader", fmt.Sprintf("%d %T", i, r),
				"addr", address.String(), "loc", key.String(), "expected", so, "got", s)
		}
	}
	return so, nil
}

func (m *MultiStateReader) ReadAccountCode(address common.Address, incarnation uint64, codeHash common.Hash) ([]byte, error) {
	var so []byte
	for i, r := range m.readers {
		s, err := r.ReadAccountCode(address, incarnation, codeHash)
		if err != nil {
			return nil, err
		}
		if i == 0 {
			so = common.Copy(s)
		}
		if !m.compare {
			continue
		}
		if !bytes.Equal(so, s) {
			log.Warn("state code invalid read",
				"reader", fmt.Sprintf("%d %T", i, r),
				"addr", address.String(), "expected", so, "got", s)
		}
	}
	return so, nil
}

func (m *MultiStateReader) ReadAccountCodeSize(address common.Address, incarnation uint64, codeHash common.Hash) (int, error) {
	var so int
	for i, r := range m.readers {
		s, err := r.ReadAccountCodeSize(address, incarnation, codeHash)
		if err != nil {
			return 0, err
		}
		if i == 0 {
			so = s
		}
		if !m.compare {
			continue
		}
		if so != s {
			log.Warn("state code size invalid read",
				"reader", fmt.Sprintf("%d %T", i, r),
				"addr", address.String(), "expected", so, "got", s)
		}
	}
	return so, nil
}

func (m *MultiStateReader) ReadAccountIncarnation(address common.Address) (uint64, error) {
	var so uint64
	for i, r := range m.readers {
		s, err := r.ReadAccountIncarnation(address)
		if err != nil {
			return 0, err
		}
		if i == 0 {
			so = s
		}
		if !m.compare {
			continue
		}
		if so != s {
			log.Warn("state incarnation invalid read",
				"reader", fmt.Sprintf("%d %T", i, r),
				"addr", address.String(), "expected", so, "got", s)
		}
	}
	return so, nil
}

type Update4ReadWriter struct {
	updates *state.UpdateTree

	initPatriciaState sync.Once

	patricia     commitment.Trie
	commitment   *btree2.Map[string, []byte]
	branchMerger *commitment.BranchMerger
	domains      *state.SharedDomains
	writes       []commitment.Update
	reads        []commitment.Update
}

func NewUpdate4ReadWriter(domains *state.SharedDomains) *Update4ReadWriter {
	return &Update4ReadWriter{
		updates:      state.NewUpdateTree(),
		domains:      domains,
		commitment:   btree2.NewMap[string, []byte](128),
		branchMerger: commitment.NewHexBranchMerger(8192),
		patricia:     commitment.InitializeTrie(commitment.VariantHexPatriciaTrie),
	}
}

func (w *Update4ReadWriter) UpdateAccountData(address common.Address, original, account *accounts.Account) error {
	//fmt.Printf("account [%x]=>{Balance: %d, Nonce: %d, Root: %x, CodeHash: %x} txNum: %d\n", address, &account.Balance, account.Nonce, account.Root, account.CodeHash, w.txNum)
	//w.updates.TouchPlainKey(address.Bytes(), accounts.SerialiseV3(account), w.updates.TouchAccount)
	w.updates.TouchPlainKeyDom(w.domains, address.Bytes(), accounts.SerialiseV3(account), w.updates.TouchAccount)
	return nil
}

func (w *Update4ReadWriter) UpdateAccountCode(address common.Address, incarnation uint64, codeHash common.Hash, code []byte) error {
	//addressBytes, codeHashBytes := address.Bytes(), codeHash.Bytes()
	//fmt.Printf("code [%x] => [%x] CodeHash: %x, txNum: %d\n", address, code, codeHash, w.txNum)
	//w.updates.TouchPlainKey(address.Bytes(), code, w.updates.TouchCode)
	w.updates.TouchPlainKeyDom(w.domains, address.Bytes(), code, w.updates.TouchCode)
	return nil
}

func (w *Update4ReadWriter) DeleteAccount(address common.Address, original *accounts.Account) error {
	addressBytes := address.Bytes()
	//w.updates.TouchPlainKey(addressBytes, nil, w.updates.TouchAccount)
	w.updates.TouchPlainKeyDom(w.domains, addressBytes, nil, w.updates.TouchAccount)
	return nil
}

func (w *Update4ReadWriter) accountFn(plainKey []byte, cell *commitment.Cell) error {
	item, found := w.updates.Get(plainKey)
	if found {
		upd := item.Update()

		cell.Nonce = upd.Nonce
		cell.Balance.Set(&upd.Balance)
		if upd.ValLength == length.Hash {
			copy(cell.CodeHash[:], upd.CodeHashOrStorage[:])
		}
	}
	return w.domains.AccountFn(plainKey, cell)
}

func (w *Update4ReadWriter) storageFn(plainKey []byte, cell *commitment.Cell) error {
	item, found := w.updates.Get(plainKey)
	if found {
		upd := item.Update()
		cell.StorageLen = upd.ValLength
		copy(cell.Storage[:], upd.CodeHashOrStorage[:upd.ValLength])
		cell.Delete = cell.StorageLen == 0
	}
	return w.domains.StorageFn(plainKey, cell)

}

func (w *Update4ReadWriter) branchFn(key []byte) ([]byte, error) {
	b, ok := w.commitment.Get(string(key))
	if !ok {
		return w.domains.BranchFn(key)
	}
	return b, nil
}

// CommitmentUpdates returns the commitment updates for the current state of w.updates.
// Commitment is based on sharedDomains commitment tree
// All branch changes are stored inside Update4ReadWriter in commitment map.
// Those updates got priority over sharedDomains commitment updates.
func (w *Update4ReadWriter) CommitmentUpdates() ([]byte, error) {
	w.patricia.Reset()
	w.initPatriciaState.Do(func() {
		// get commitment state from commitment domain (like we're adding updates to it)
		stateBytes, err := w.domains.Commitment.PatriciaState()
		if err != nil {
			panic(err)
		}
		switch pt := w.patricia.(type) {
		case *commitment.HexPatriciaHashed:
			if err := pt.SetState(stateBytes); err != nil {
				panic(fmt.Errorf("set HPH state: %w", err))
			}
			rh, err := pt.RootHash()
			if err != nil {
				panic(fmt.Errorf("HPH root hash: %w", err))
			}
			fmt.Printf("HPH state set: %x\n", rh)
		default:
			panic(fmt.Errorf("unsupported patricia type: %T", pt))
		}
	})

	w.patricia.ResetFns(w.branchFn, w.accountFn, w.storageFn)
	rh, branches, err := w.patricia.ProcessUpdates(w.updates.List(false))
	if err != nil {
		return nil, err
	}
	for k, update := range branches {
		//w.commitment.Set(k, b)
		prefix := []byte(k)

		stateValue, err := w.branchFn(prefix)
		if err != nil {
			return nil, err
		}
		stated := commitment.BranchData(stateValue)
		merged, err := w.branchMerger.Merge(stated, update)
		if err != nil {
			return nil, err
		}
		if bytes.Equal(stated, merged) {
			continue
		}
		w.commitment.Set(hex.EncodeToString(prefix), merged)
	}
	return rh, nil
}

func (w *Update4ReadWriter) WriteAccountStorage(address common.Address, incarnation uint64, key *common.Hash, original, value *uint256.Int) error {
	if original.Eq(value) {
		return nil
	}
	//fmt.Printf("storage [%x] [%x] => [%x], txNum: %d\n", address, *key, v, w.txNum)
	//w.updates.TouchPlainKey(common.Append(address[:], key[:]), value.Bytes(), w.updates.TouchStorage)
	w.updates.TouchPlainKeyDom(w.domains, common.Append(address[:], key[:]), value.Bytes(), w.updates.TouchStorage)
	return nil
}

func (w *Update4ReadWriter) Updates() (pk [][]byte, upd []commitment.Update) {
	pk, _, updates := w.updates.List(true)
	return pk, updates
}

func (w *Update4ReadWriter) CreateContract(address common.Address) error { return nil }

func UpdateToAccount(u commitment.Update) *accounts.Account {
	acc := accounts.NewAccount()
	acc.Initialised = true
	acc.Balance.Set(&u.Balance)
	acc.Nonce = u.Nonce
	if u.ValLength > 0 {
		acc.CodeHash = common.BytesToHash(u.CodeHashOrStorage[:u.ValLength])
	}
	return &acc
}

func (w *Update4ReadWriter) ReadAccountData(address common.Address) (*accounts.Account, error) {
	ci, found := w.updates.Get(address.Bytes())
	if !found {
		return nil, nil
	}

	upd := ci.Update()
	w.reads = append(w.reads, upd)
	return UpdateToAccount(upd), nil
}

func (w *Update4ReadWriter) ReadAccountStorage(address common.Address, incarnation uint64, key *common.Hash) ([]byte, error) {
	ci, found := w.updates.Get(common.Append(address.Bytes(), key.Bytes()))
	if !found {
		return nil, nil
	}
	upd := ci.Update()
	w.reads = append(w.reads, upd)

	if upd.ValLength > 0 {
		return upd.CodeHashOrStorage[:upd.ValLength], nil
	}
	return nil, nil
}

func (w *Update4ReadWriter) ReadAccountCode(address common.Address, incarnation uint64, codeHash common.Hash) ([]byte, error) {
	ci, found := w.updates.Get(address.Bytes())
	if !found {
		return nil, nil
	}
	upd := ci.Update()
	w.reads = append(w.reads, upd)
	if upd.ValLength > 0 {
		return upd.CodeHashOrStorage[:upd.ValLength], nil
	}
	return nil, nil
}

func (w *Update4ReadWriter) ReadAccountCodeSize(address common.Address, incarnation uint64, codeHash common.Hash) (int, error) {
	c, err := w.ReadAccountCode(address, incarnation, codeHash)
	if err != nil {
		return 0, err
	}
	return len(c), nil
}

func (w *Update4ReadWriter) ReadAccountIncarnation(address common.Address) (uint64, error) {
	return 0, nil
}