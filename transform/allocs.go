package transform

// This file implements an escape analysis pass. It looks for calls to
// runtime.alloc and replaces these calls with a stack allocation if the
// allocated value does not escape. It uses the LLVM nocapture flag for
// interprocedural escape analysis.

import (
	"github.com/tinygo-org/tinygo/llvmutil"
	"tinygo.org/x/go-llvm"
)

// maxStackAlloc is the maximum size of an object that will be allocated on the
// stack. Bigger objects have increased risk of stack overflows and thus will
// always be heap allocated.
//
// TODO: tune this, this is just a random value.
const maxStackAlloc = 256

// OptimizeAllocs tries to replace heap allocations with stack allocations
// whenever possible. It relies on the LLVM 'nocapture' flag for interprocedural
// escape analysis, and within a function looks whether an allocation can escape
// to the heap.
func OptimizeAllocs(mod llvm.Module) {
	allocator := mod.NamedFunction("runtime.alloc")
	if allocator.IsNil() {
		// nothing to optimize
		return
	}

	targetData := llvm.NewTargetData(mod.DataLayout())
	i8ptrType := llvm.PointerType(mod.Context().Int8Type(), 0)
	builder := mod.Context().NewBuilder()

	for _, heapalloc := range llvmutil.GetUses(allocator) {
		if heapalloc.Operand(0).IsAConstant().IsNil() {
			// Do not allocate variable length arrays on the stack.
			continue
		}

		size := heapalloc.Operand(0).ZExtValue()
		if size > maxStackAlloc {
			// The maximum size for a stack allocation.
			continue
		}

		// In general the pattern is:
		//     %0 = call i8* @runtime.alloc(i32 %size)
		//     %1 = bitcast i8* %0 to type*
		//     (use %1 only)
		// But the bitcast might sometimes be dropped when allocating an *i8.
		// The 'bitcast' variable below is thus usually a bitcast of the
		// heapalloc but not always.
		bitcast := heapalloc // instruction that creates the value
		if uses := llvmutil.GetUses(heapalloc); len(uses) == 1 && !uses[0].IsABitCastInst().IsNil() {
			// getting only bitcast use
			bitcast = uses[0]
		}

		if mayEscape(bitcast) {
			continue
		}
		// The pointer value does not escape.

		// Insert alloca in the entry block. Do it here so that mem2reg can
		// promote it to a SSA value.
		fn := bitcast.InstructionParent().Parent()
		builder.SetInsertPointBefore(fn.EntryBasicBlock().FirstInstruction())
		alignment := targetData.ABITypeAlignment(i8ptrType)
		sizeInWords := (size + uint64(alignment) - 1) / uint64(alignment)
		allocaType := llvm.ArrayType(mod.Context().IntType(alignment*8), int(sizeInWords))
		alloca := builder.CreateAlloca(allocaType, "stackalloc.alloca")
		zero := llvmutil.GetZeroValue(alloca.Type().ElementType())
		builder.CreateStore(zero, alloca)
		stackalloc := builder.CreateBitCast(alloca, bitcast.Type(), "stackalloc")
		bitcast.ReplaceAllUsesWith(stackalloc)
		if heapalloc != bitcast {
			bitcast.EraseFromParentAsInstruction()
		}
		heapalloc.EraseFromParentAsInstruction()
	}
}

// mayEscape returns whether the value might escape. It returns true if it might
// escape, and false if it definitely doesn't. The value must be an instruction.
func mayEscape(value llvm.Value) bool {
	uses := llvmutil.GetUses(value)
	for _, use := range uses {
		if use.IsAInstruction().IsNil() {
			panic("expected instruction use")
		}
		switch use.InstructionOpcode() {
		case llvm.GetElementPtr:
			if mayEscape(use) {
				return true
			}
		case llvm.BitCast:
			// A bitcast escapes if the casted-to value escapes.
			if mayEscape(use) {
				return true
			}
		case llvm.Load:
			// Load does not escape.
		case llvm.Store:
			// Store only escapes when the value is stored to, not when the
			// value is stored into another value.
			if use.Operand(0) == value {
				return true
			}
		case llvm.Call:
			if !hasFlag(use, value, "nocapture") {
				return true
			}
		case llvm.ICmp:
			// Comparing pointers don't let the pointer escape.
			// This is often a compiler-inserted nil check.
		default:
			// Unknown instruction, might escape.
			return true
		}
	}

	// Checked all uses, and none let the pointer value escape.
	return false
}
