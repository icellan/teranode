package blockchain

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// pipelinePublicBlockchainAPIMethods are the members of publicBlockchainAPIMethods
// that are deliberately public AND mutating: Teranode's own services call them
// in the normal course of following the chain (see the classification comment
// on protectedMethods in Server.go), and protecting them would brick a default
// (grpc_admin_api_key unset) deployment. They are exempt from
// TestPublicBlockchainAPIMethodsAreReadOnly - the analogue of p2p's
// protectedWithoutGuardedWrites/ConnectPeer-DisconnectPeer exemption, just
// inverted: here it is public RPCs allowed to write.
var pipelinePublicBlockchainAPIMethods = map[string]bool{
	"/blockchain_api.BlockchainAPI/AddBlock":                   true,
	"/blockchain_api.BlockchainAPI/SetState":                   true,
	"/blockchain_api.BlockchainAPI/AssignBlockID":              true,
	"/blockchain_api.BlockchainAPI/GetNextBlockID":             true,
	"/blockchain_api.BlockchainAPI/SetBlockMinedSet":           true,
	"/blockchain_api.BlockchainAPI/ClearBlockMinedSet":         true,
	"/blockchain_api.BlockchainAPI/SetBlockSubtreesSet":        true,
	"/blockchain_api.BlockchainAPI/SetBlockPersistedAt":        true,
	"/blockchain_api.BlockchainAPI/SetBlockProcessedAt":        true,
	"/blockchain_api.BlockchainAPI/SendFSMEvent":               true,
	"/blockchain_api.BlockchainAPI/Run":                        true,
	"/blockchain_api.BlockchainAPI/Idle":                       true,
	"/blockchain_api.BlockchainAPI/CatchUpBlocks":              true,
	"/blockchain_api.BlockchainAPI/ScheduleBlobDeletion":       true,
	"/blockchain_api.BlockchainAPI/CancelBlobDeletion":         true,
	"/blockchain_api.BlockchainAPI/RemoveBlobDeletion":         true,
	"/blockchain_api.BlockchainAPI/IncrementBlobDeletionRetry": true,
	"/blockchain_api.BlockchainAPI/CompleteBlobDeletions":      true,
	"/blockchain_api.BlockchainAPI/AcquireBlobDeletionBatch":   true,
	"/blockchain_api.BlockchainAPI/CompleteBlobDeletionBatch":  true,
}

// blockchainReadOnlyStoreMethods lists the blockchain_store.Store methods that
// only read, verified against every (*Blockchain) handler and same-package
// helper function that TestPublicBlockchainAPIMethodsAreReadOnly walks. A
// method not on this list counts as a write - so a new store method must be
// added here deliberately, not by pattern-matching its name, the same
// discipline protectedMethods documents for RPCs.
var blockchainReadOnlyStoreMethods = map[string]bool{
	"GetBlock":                             true,
	"GetBlocks":                            true,
	"GetBlockByHeight":                     true,
	"GetBlockByID":                         true,
	"GetBlockStats":                        true,
	"GetBlockGraphData":                    true,
	"GetLastNBlocks":                       true,
	"GetLastNInvalidBlocks":                true,
	"GetSuitableBlock":                     true,
	"GetHashOfAncestorBlock":               true,
	"GetLatestBlockHeaderFromBlockLocator": true,
	"GetBlockHeadersFromOldest":            true,
	"GetBlockHeader":                       true,
	"GetBlockExists":                       true,
	"GetBestBlockHeader":                   true,
	"CheckBlockIsInCurrentChain":           true,
	"CheckBlockIsAncestorOfBlock":          true,
	"GetChainTips":                         true,
	"GetBlockHeaders":                      true,
	"GetBlockHeadersFromTill":              true,
	"GetBlockHeadersFromHeight":            true,
	"GetBlockHeadersByHeight":              true,
	"GetBlocksByHeight":                    true,
	"FindBlocksContainingSubtree":          true,
	"GetBlockHeaderIDs":                    true,
	"GetState":                             true,
	"GetBlockIsMined":                      true,
	"GetBlocksMinedNotSet":                 true,
	"GetBlocksNotPersisted":                true,
	"GetBlocksSubtreesNotSet":              true,
	"LocateBlockHeaders":                   true,
	"GetBlockInChainByHeightHash":          true, // used by getBlockLocatorByWalk
	"MainChainBlockHashesByHeights":        true, // used by getBlockLocator
	"ListScheduledBlobDeletions":           true, // narrower interface asserted from b.store in ListScheduledDeletions
	"GetPendingBlobDeletions":              true, // narrower interface asserted from b.store in GetPendingBlobDeletions
}

// TestPublicBlockchainAPIMethodsAreReadOnly proves from the handler source
// that every BlockchainAPI RPC left in publicBlockchainAPIMethods but not in
// pipelinePublicBlockchainAPIMethods is genuinely read-only. Moving a mutating
// RPC to the public list (Fix 1) makes "a mutating RPC ends up public" the
// live risk; without this, the classification drifts the moment somebody adds
// a write to an existing public handler, and an unauthenticated caller
// inherits it. Mirrors services/p2p/server_auth_test.go's
// TestPublicRPCsDoNotMutateRegistry, scoped to the one guarded field that
// matters here: b.store.
func TestPublicBlockchainAPIMethodsAreReadOnly(t *testing.T) {
	fns := parseBlockchainPackageFuncs(t)

	for method := range publicBlockchainAPIMethods {
		if pipelinePublicBlockchainAPIMethods[method] {
			continue
		}

		name := method[strings.LastIndex(method, "/")+1:]

		if name == "HealthGRPC" {
			continue // no store access at all
		}

		decl, ok := fns["Blockchain."+name]
		require.True(t, ok, "no (*Blockchain).%s handler found for public RPC %s", name, method)

		writes := findStoreWrites(fns, decl, map[string]bool{})
		require.Empty(t, writes,
			"public RPC %s calls a store method not on the verified read-only allow-list (%s); reclassify it as protected/pipeline or extend blockchainReadOnlyStoreMethods",
			method, strings.Join(writes, ", "))
	}
}

// parseBlockchainPackageFuncs indexes every non-test function in this package
// by "Blockchain.<name>" for methods on *Blockchain, or "<name>" for plain
// functions (the free getBlockLocator/getBlockHeadersToCommonAncestor/
// getBlockHeadersFromCommonAncestor helpers that public handlers delegate to).
func parseBlockchainPackageFuncs(t *testing.T) map[string]*ast.FuncDecl {
	t.Helper()

	entries, err := os.ReadDir(".")
	require.NoError(t, err)

	fset := token.NewFileSet()
	fns := make(map[string]*ast.FuncDecl)

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}

		file, err := parser.ParseFile(fset, name, nil, 0)
		require.NoError(t, err)

		for _, d := range file.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}

			if fn.Recv == nil {
				fns[fn.Name.Name] = fn
				continue
			}

			if isBlockchainMethod(fn) {
				fns["Blockchain."+fn.Name.Name] = fn
			}
		}
	}

	return fns
}

// isBlockchainType reports whether an AST type expression is Blockchain or
// *Blockchain.
func isBlockchainType(expr ast.Expr) bool {
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}

	ident, ok := expr.(*ast.Ident)

	return ok && ident.Name == "Blockchain"
}

// isBlockchainMethod reports whether fn is a method on Blockchain or
// *Blockchain (as opposed to Client, LocalClient, Mock, or another type in the
// package that happens to share a method name).
func isBlockchainMethod(fn *ast.FuncDecl) bool {
	return fn.Recv != nil && len(fn.Recv.List) == 1 && isBlockchainType(fn.Recv.List[0].Type)
}

// isStoreType reports whether an AST type expression is blockchain_store.Store.
func isStoreType(expr ast.Expr) bool {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok {
		return false
	}

	ident, ok := sel.X.(*ast.Ident)

	return ok && ident.Name == "blockchain_store" && sel.Sel.Name == "Store"
}

// storeAccess describes how fn can reach the guarded store value: as the
// "store" field on its *Blockchain receiver, and/or as a parameter of type
// blockchain_store.Store (the getBlockLocator/getBlockHeadersToCommonAncestor/
// getBlockHeadersFromCommonAncestor helpers all take one).
type storeAccess struct {
	recvIdent  string // non-empty: receiver identifier name, guarded via <recvIdent>.store
	paramIdent string // non-empty: parameter identifier name that is itself the store value
}

func storeAccessFor(fn *ast.FuncDecl) storeAccess {
	var sa storeAccess

	if fn.Recv != nil && len(fn.Recv.List) == 1 && isBlockchainType(fn.Recv.List[0].Type) {
		if names := fn.Recv.List[0].Names; len(names) == 1 {
			sa.recvIdent = names[0].Name
		}
	}

	if fn.Type.Params != nil {
		for _, field := range fn.Type.Params.List {
			if !isStoreType(field.Type) {
				continue
			}

			for _, n := range field.Names {
				if n.Name != "_" {
					sa.paramIdent = n.Name
				}
			}
		}
	}

	return sa
}

// isGuardedStoreExpr reports whether expr is the guarded store value under sa:
// either `<recvIdent>.store` or the bare store parameter identifier.
func isGuardedStoreExpr(expr ast.Expr, sa storeAccess) bool {
	if sa.paramIdent != "" {
		if ident, ok := expr.(*ast.Ident); ok && ident.Name == sa.paramIdent {
			return true
		}
	}

	if sa.recvIdent != "" {
		if sel, ok := expr.(*ast.SelectorExpr); ok {
			if ident, ok := sel.X.(*ast.Ident); ok && ident.Name == sa.recvIdent && sel.Sel.Name == "store" {
				return true
			}
		}
	}

	return false
}

// findStoreWrites walks fn and every same-package function it calls, and
// reports each store method call that is not on blockchainReadOnlyStoreMethods.
//
// The guard is a source-level approximation, not a proof (see the same caveat
// on services/p2p/server_auth_test.go's findGuardedWrites): it follows direct
// method/function calls within this package only. It is enough to keep the
// verified read-only public methods from silently growing a write.
func findStoreWrites(fns map[string]*ast.FuncDecl, fn *ast.FuncDecl, seen map[string]bool) []string {
	sa := storeAccessFor(fn)

	var writes []string

	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}

		sel, ok := call.Fun.(*ast.SelectorExpr)
		if ok {
			if isGuardedStoreExpr(sel.X, sa) {
				if !blockchainReadOnlyStoreMethods[sel.Sel.Name] {
					writes = append(writes, fn.Name.Name+" -> store."+sel.Sel.Name)
				}

				return true
			}

			// A call on the *Blockchain receiver itself (b.SomeOtherMethod(...))
			// may reach the store indirectly; recurse into it.
			if sa.recvIdent != "" {
				if ident, ok := sel.X.(*ast.Ident); ok && ident.Name == sa.recvIdent {
					writes = append(writes, callee(fns, "Blockchain."+sel.Sel.Name, seen)...)
				}
			}

			return true
		}

		// A free-function call (e.g. getBlockLocator(ctx, b.store, ...)).
		if ident, ok := call.Fun.(*ast.Ident); ok {
			writes = append(writes, callee(fns, ident.Name, seen)...)
		}

		return true
	})

	return writes
}

// callee recurses into a same-package function, guarding against cycles.
func callee(fns map[string]*ast.FuncDecl, key string, seen map[string]bool) []string {
	if seen[key] {
		return nil
	}

	target, ok := fns[key]
	if !ok {
		return nil
	}

	seen[key] = true

	return findStoreWrites(fns, target, seen)
}
