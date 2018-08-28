package node

import (
	"context"

	"gx/ipfs/QmSkuaNgyGmV8c1L3cZNWcUxRJV6J3nsD96JVQPcWcwtyW/go-hamt-ipld"
	"gx/ipfs/QmVmDhyTTUcQXFD1rRQ64fGLMSAoaQvNH3hwuaCFAPq2hy/errors"
	offline "gx/ipfs/QmWdao8WJqYU65ZbYQyQWMFqku6QFxkPiv8HSUAkXdHZoe/go-ipfs-exchange-offline"
	bstore "gx/ipfs/QmcD7SqfyQyA91TZUQ7VPRYbGarxmY7EsQewVYMuN5LNSv/go-ipfs-blockstore"
	ci "gx/ipfs/Qme1knMqwt1hKZbc1BmQFmnm9f36nyQGwXxPGVpVJ9rMK5/go-libp2p-crypto"

	bserv "gx/ipfs/QmUSuYd5Q1N291DH679AVvHwGLwtS1V9VPDWvnUN9nGJPT/go-blockservice"

	"github.com/filecoin-project/go-filecoin/core"
	"github.com/filecoin-project/go-filecoin/repo"
	"github.com/filecoin-project/go-filecoin/types"
	"github.com/filecoin-project/go-filecoin/wallet"
)

var ErrLittleBits = errors.New("Bitsize less than 1024 is considered unsafe") // nolint: golint

// InitCfg contains configuration for initializing a node
type InitCfg struct {
	PeerKey ci.PrivKey
}

// InitOpt is an init option function
type InitOpt func(*InitCfg)

// PrivKeyOpt sets the private key for the nodes 'self' key
// this is the key that is used for libp2p identity
func PrivKeyOpt(k ci.PrivKey) InitOpt {
	return func(c *InitCfg) {
		c.PeerKey = k
	}
}

// Init initializes a filecoin node in the given repo
// TODO: accept options?
//  - configurable genesis block
func Init(ctx context.Context, r repo.Repo, gen core.GenesisInitFunc, opts ...InitOpt) error {
	cfg := new(InitCfg)
	for _, o := range opts {
		o(cfg)
	}

	// TODO(ipfs): make the blockstore and blockservice have the same interfaces
	// so that this becomes less painful
	bs := bstore.NewBlockstore(r.Datastore())
	cst := &hamt.CborIpldStore{Blocks: bserv.New(bs, offline.Exchange(bs))}

	cm := core.NewChainManager(r.Datastore(), bs, cst)
	if err := cm.Genesis(ctx, gen); err != nil {
		return errors.Wrap(err, "failed to initialize genesis")
	}

	if cfg.PeerKey == nil {
		// TODO: make size configurable
		sk, err := makePrivateKey(2048)
		if err != nil {
			return errors.Wrap(err, "failed to create nodes private key")
		}

		cfg.PeerKey = sk
	}

	if err := r.Keystore().Put("self", cfg.PeerKey); err != nil {
		return errors.Wrap(err, "failed to store private key")
	}

	// TODO: do we want this?
	// TODO: but behind a config option if this should be generated
	if r.Config().Wallet.DefaultAddress == (types.Address{}) {
		addr, err := newAddress(r)
		if err != nil {
			return errors.Wrap(err, "failed to generate default address")
		}

		newConfig := r.Config()
		newConfig.Wallet.DefaultAddress = addr
		if err := r.ReplaceConfig(newConfig); err != nil {
			return errors.Wrap(err, "failed to update config")
		}
	}
	return nil
}

// borrowed from go-ipfs: `repo/config/init.go`
func makePrivateKey(nbits int) (ci.PrivKey, error) {
	if nbits < 1024 {
		return nil, ErrLittleBits
	}

	// create a public private key pair
	sk, _, err := ci.GenerateKeyPair(ci.RSA, nbits)
	if err != nil {
		return nil, err
	}

	return sk, nil
}

func newAddress(r repo.Repo) (types.Address, error) {
	backend, err := wallet.NewDSBackend(r.WalletDatastore())
	if err != nil {
		return types.Address{}, errors.Wrap(err, "failed to set up wallet backend")
	}

	return backend.NewAddress()
}
