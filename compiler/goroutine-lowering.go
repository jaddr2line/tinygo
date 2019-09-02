package compiler

// This file implements lowering for the goroutine scheduler. There are two
// scheduler implementations, one based on tasks (like RTOSes and the main Go
// runtime) and one based on a coroutine compiler transformation. The task based
// implementation requires very little work from the compiler but is not very
// portable (in particular, it is very hard if not impossible to support on
// WebAssembly). The coroutine based one requires a lot of work by the compiler
// to implement, but can run virtually anywhere with a single scheduler
// implementation.
//
// The below description is for the coroutine based scheduler.
//
// This file lowers goroutine pseudo-functions into coroutines scheduled by a
// scheduler at runtime. It uses coroutine support in LLVM for this
// transformation: https://llvm.org/docs/Coroutines.html
//
// For example, take the following code:
//
//     func main() {
//         go foo()
//         time.Sleep(2 * time.Second)
//         println("some other operation")
//         i := bar()
//         println("done", *i)
//     }
//
//     func foo() {
//         for {
//             println("foo!")
//             time.Sleep(time.Second)
//         }
//     }
//
//     func bar() *int {
//         time.Sleep(time.Second)
//         println("blocking operation completed)
//         return new(int)
//     }
//
// It is transformed by the IR generator in compiler.go into the following
// pseudo-Go code:
//
//     func main() {
//         fn := runtime.makeGoroutine(foo)
//         fn()
//         time.Sleep(2 * time.Second)
//         println("some other operation")
//         i := bar() // imagine an 'await' keyword in front of this call
//         println("done", *i)
//     }
//
//     func foo() {
//         for {
//             println("foo!")
//             time.Sleep(time.Second)
//         }
//     }
//
//     func bar() *int {
//         time.Sleep(time.Second)
//         println("blocking operation completed)
//         return new(int)
//     }
//
// The pass in this file transforms this code even further, to the following
// async/await style pseudocode:
//
//     func main(parent) {
//         hdl := llvm.makeCoroutine()
//         foo(nil)                                // do not pass the parent coroutine: this is an independent goroutine
//         runtime.sleepTask(hdl, 2 * time.Second) // ask the scheduler to re-activate this coroutine at the right time
//         llvm.suspend(hdl)                       // suspend point
//         println("some other operation")
//         var i *int                              // allocate space on the stack for the return value
//         runtime.setTaskStatePtr(hdl, &i)        // store return value alloca in our coroutine promise
//         bar(hdl)                                // await, pass a continuation (hdl) to bar
//         llvm.suspend(hdl)                       // suspend point, wait for the callee to re-activate
//         println("done", *i)
//         runtime.activateTask(parent)            // re-activate the parent (nop, there is no parent)
//     }
//
//     func foo(parent) {
//         hdl := llvm.makeCoroutine()
//         for {
//             println("foo!")
//             runtime.sleepTask(hdl, time.Second) // ask the scheduler to re-activate this coroutine at the right time
//             llvm.suspend(hdl)                   // suspend point
//         }
//     }
//
//     func bar(parent) {
//         hdl := llvm.makeCoroutine()
//         runtime.sleepTask(hdl, time.Second) // ask the scheduler to re-activate this coroutine at the right time
//         llvm.suspend(hdl)                   // suspend point
//         println("blocking operation completed)
//         runtime.activateTask(parent)        // re-activate the parent coroutine before returning
//     }
//
// The real LLVM code is more complicated, but this is the general idea.
//
// The LLVM coroutine passes will then process this file further transforming
// these three functions into coroutines. Most of the actual work is done by the
// scheduler, which runs in the background scheduling all coroutines.

import (
	"errors"
	"fmt"
	"strings"

	"tinygo.org/x/go-llvm"
)

type asyncFunc struct {
	taskHandle   llvm.Value
	cleanupBlock llvm.BasicBlock
	suspendBlock llvm.BasicBlock
}

// LowerGoroutines performs some IR transformations necessary to support
// goroutines. It does something different based on whether it uses the
// coroutine or the tasks implementation of goroutines, and whether goroutines
// are necessary at all.
func (c *Compiler) LowerGoroutines() error {
	switch c.selectScheduler() {
	case "coroutines":
		return c.lowerCoroutines()
	case "tasks":
		return c.lowerTasks()
	default:
		panic("unknown scheduler type")
	}
}

// lowerTasks starts the main goroutine and then runs the scheduler.
// This is enough compiler-level transformation for the task-based scheduler.
func (c *Compiler) lowerTasks() error {
	uses := getUses(c.mod.NamedFunction("runtime.callMain"))
	if len(uses) != 1 || uses[0].IsACallInst().IsNil() {
		panic("expected exactly 1 call of runtime.callMain, check the entry point")
	}
	mainCall := uses[0]

	realMain := c.mod.NamedFunction(c.ir.MainPkg().Pkg.Path() + ".main")
	if len(getUses(c.mod.NamedFunction("runtime.startGoroutine"))) != 0 {
		// Program needs a scheduler. Start main.main as a goroutine and start
		// the scheduler.
		realMainWrapper := c.createGoroutineStartWrapper(realMain)
		c.builder.SetInsertPointBefore(mainCall)
		zero := llvm.ConstInt(c.uintptrType, 0, false)
		c.createRuntimeCall("startGoroutine", []llvm.Value{realMainWrapper, zero}, "")
		c.createRuntimeCall("scheduler", nil, "")
	} else {
		// Program doesn't need a scheduler. Call main.main directly.
		c.builder.SetInsertPointBefore(mainCall)
		params := []llvm.Value{
			llvm.Undef(c.i8ptrType), // unused context parameter
			llvm.Undef(c.i8ptrType), // unused coroutine handle
		}
		c.createCall(realMain, params, "")
	}
	mainCall.EraseFromParentAsInstruction()

	// main.main was set to external linkage during IR construction. Set it to
	// internal linkage to enable interprocedural optimizations.
	realMain.SetLinkage(llvm.InternalLinkage)

	return nil
}

// lowerCoroutines transforms the IR into one where all blocking functions are
// turned into goroutines and blocking calls into await calls. It also makes
// sure that the first coroutine is started and the coroutine scheduler will be
// run.
func (c *Compiler) lowerCoroutines() error {
	needsScheduler, err := c.markAsyncFunctions()
	if err != nil {
		return err
	}

	uses := getUses(c.mod.NamedFunction("runtime.callMain"))
	if len(uses) != 1 || uses[0].IsACallInst().IsNil() {
		panic("expected exactly 1 call of runtime.callMain, check the entry point")
	}
	mainCall := uses[0]

	// Replace call of runtime.callMain() with a real call to main.main(),
	// optionally followed by a call to runtime.scheduler().
	c.builder.SetInsertPointBefore(mainCall)
	realMain := c.mod.NamedFunction(c.ir.MainPkg().Pkg.Path() + ".main")
	c.builder.CreateCall(realMain, []llvm.Value{llvm.Undef(c.i8ptrType), c.createRuntimeCall("getFakeCoroutine", []llvm.Value{}, "")}, "")
	if needsScheduler {
		c.createRuntimeCall("scheduler", nil, "")
	}
	mainCall.EraseFromParentAsInstruction()

	if !needsScheduler {
		go_scheduler := c.mod.NamedFunction("go_scheduler")
		if !go_scheduler.IsNil() {
			// This is the WebAssembly backend.
			// There is no need to export the go_scheduler function, but it is
			// still exported. Make sure it is optimized away.
			go_scheduler.SetLinkage(llvm.InternalLinkage)
		}
	}

	// main.main was set to external linkage during IR construction. Set it to
	// internal linkage to enable interprocedural optimizations.
	realMain.SetLinkage(llvm.InternalLinkage)

	return nil
}

const coroDebug = false

func coroDebugPrintln(s ...interface{}) {
	if coroDebug {
		fmt.Println(s...)
	}
}

// markAsyncFunctions does the bulk of the work of lowering goroutines. It
// determines whether a scheduler is needed, and if it is, it transforms
// blocking operations into goroutines and blocking calls into await calls.
//
// It does the following operations:
//    * Find all blocking functions.
//    * Determine whether a scheduler is necessary. If not, it skips the
//      following operations.
//    * Transform call instructions into await calls.
//    * Transform return instructions into final suspends.
//    * Set up the coroutine frames for async functions.
//    * Transform blocking calls into their async equivalents.
func (c *Compiler) markAsyncFunctions() (needsScheduler bool, err error) {
	var worklist []llvm.Value

	yield := c.mod.NamedFunction("runtime.yield")
	if !yield.IsNil() {
		worklist = append(worklist, yield)
	}

	if len(worklist) == 0 {
		// There are no blocking operations, so no need to transform anything.
		return false, c.lowerMakeGoroutineCalls(nil)
	}

	// Find all async functions.
	// Keep reducing this worklist by marking a function as recursively async
	// from the worklist and pushing all its parents that are non-async.
	// This is somewhat similar to a worklist in a mark-sweep garbage collector:
	// the work items are then grey objects.
	asyncFuncs := make(map[llvm.Value]*asyncFunc)
	asyncList := make([]llvm.Value, 0, 4)
	for len(worklist) != 0 {
		// Pick the topmost.
		f := worklist[len(worklist)-1]
		worklist = worklist[:len(worklist)-1]
		if _, ok := asyncFuncs[f]; ok {
			continue // already processed
		}
		if f.Name() == "resume" {
			continue
		}
		// Add to set of async functions.
		asyncFuncs[f] = &asyncFunc{}
		asyncList = append(asyncList, f)

		// Add all callees to the worklist.
		for _, use := range getUses(f) {
			if use.IsConstant() && use.Opcode() == llvm.PtrToInt {
				for _, call := range getUses(use) {
					if call.IsACallInst().IsNil() || call.CalledValue().Name() != "runtime.makeGoroutine" {
						return false, errors.New("async function " + f.Name() + " incorrectly used in ptrtoint, expected runtime.makeGoroutine")
					}
				}
				// This is a go statement. Do not mark the parent as async, as
				// starting a goroutine is not a blocking operation.
				continue
			}
			if use.IsConstant() && use.Opcode() == llvm.BitCast {
				// Not sure why this const bitcast is here but as long as it
				// has no uses it can be ignored, I guess?
				// I think it was created for the runtime.isnil check but
				// somehow wasn't removed when all these checks are removed.
				if len(getUses(use)) == 0 {
					continue
				}
			}
			if use.IsACallInst().IsNil() {
				// Not a call instruction. Maybe a store to a global? In any
				// case, this requires support for async calls across function
				// pointers which is not yet supported.
				return false, errors.New("async function " + f.Name() + " used as function pointer")
			}
			parent := use.InstructionParent().Parent()
			for i := 0; i < use.OperandsCount()-1; i++ {
				if use.Operand(i) == f {
					return false, errors.New("async function " + f.Name() + " used as function pointer in " + parent.Name())
				}
			}
			worklist = append(worklist, parent)
		}
	}

	// Check whether a scheduler is needed.
	makeGoroutine := c.mod.NamedFunction("runtime.makeGoroutine")
	if c.GOOS == "js" && strings.HasPrefix(c.Triple, "wasm") {
		// JavaScript always needs a scheduler, as in general no blocking
		// operations are possible. Blocking operations block the browser UI,
		// which is very bad.
		needsScheduler = true
	} else if c.GOARCH == "avr" {
		needsScheduler = false
		getCoroutine := c.mod.NamedFunction("runtime.getCoroutine")
		for _, inst := range getUses(getCoroutine) {
			inst.ReplaceAllUsesWith(llvm.Undef(inst.Type()))
			inst.EraseFromParentAsInstruction()
		}
		yield := c.mod.NamedFunction("runtime.yield")
		for _, inst := range getUses(yield) {
			inst.EraseFromParentAsInstruction()
		}
		sleep := c.mod.NamedFunction("time.Sleep")
		for _, inst := range getUses(sleep) {
			c.builder.SetInsertPointBefore(inst)
			c.createRuntimeCall("avrSleep", []llvm.Value{inst.Operand(0)}, "")
			inst.EraseFromParentAsInstruction()
		}
	} else {
		// Only use a scheduler when an async goroutine is started. When the
		// goroutine is not async (does not do any blocking operation), no
		// scheduler is necessary as it can be called directly.
		for _, use := range getUses(makeGoroutine) {
			// Input param must be const ptrtoint of function.
			ptrtoint := use.Operand(0)
			if !ptrtoint.IsConstant() || ptrtoint.Opcode() != llvm.PtrToInt {
				panic("expected const ptrtoint operand of runtime.makeGoroutine")
			}
			goroutine := ptrtoint.Operand(0)
			if _, ok := asyncFuncs[goroutine]; ok {
				needsScheduler = true
				break
			}
		}
	}

	if !needsScheduler {
		// No scheduler is needed. Do not transform all functions here.
		// However, make sure that all go calls (which are all non-async) are
		// transformed into regular calls.
		return false, c.lowerMakeGoroutineCalls(nil)
	}

	// replace indefinitely blocking yields
	getCoroutine := c.mod.NamedFunction("runtime.getCoroutine")
	coroDebugPrintln("replace indefinitely blocking yields")
	nonReturning := map[llvm.Value]bool{}
	for _, f := range asyncList {
		if f == yield {
			continue
		}
		coroDebugPrintln("scanning", f.Name())

		var callsAsyncNotYield bool
		var callsYield bool
		var getsCoroutine bool
		for bb := f.EntryBasicBlock(); !bb.IsNil(); bb = llvm.NextBasicBlock(bb) {
			for inst := bb.FirstInstruction(); !inst.IsNil(); inst = llvm.NextInstruction(inst) {
				if !inst.IsACallInst().IsNil() {
					callee := inst.CalledValue()
					if callee == yield {
						callsYield = true
					} else if callee == getCoroutine {
						getsCoroutine = true
					} else if _, ok := asyncFuncs[callee]; ok {
						callsAsyncNotYield = true
					}
				}
			}
		}

		coroDebugPrintln("result", f.Name(), callsYield, getsCoroutine, callsAsyncNotYield)

		if callsYield && !getsCoroutine && !callsAsyncNotYield {
			coroDebugPrintln("optimizing", f.Name())
			// calls yield without registering for a wakeup
			// this actually could otherwise wake up, but only in the case of really messed up undefined behavior
			// so everything after a yield is unreachable, so we can just inject a fake return
			delQueue := []llvm.Value{}
			for bb := f.EntryBasicBlock(); !bb.IsNil(); bb = llvm.NextBasicBlock(bb) {
				var broken bool

				for inst := bb.FirstInstruction(); !inst.IsNil(); inst = llvm.NextInstruction(inst) {
					if !broken && !inst.IsACallInst().IsNil() && inst.CalledValue() == yield {
						coroDebugPrintln("broke", f.Name(), bb.AsValue().Name())
						broken = true
						c.builder.SetInsertPointBefore(inst)
						c.createRuntimeCall("noret", []llvm.Value{}, "")
						if f.Type().ElementType().ReturnType().TypeKind() == llvm.VoidTypeKind {
							c.builder.CreateRetVoid()
						} else {
							c.builder.CreateRet(llvm.Undef(f.Type().ElementType().ReturnType()))
						}
					}
					if broken {
						if inst.Type().TypeKind() != llvm.VoidTypeKind {
							inst.ReplaceAllUsesWith(llvm.Undef(inst.Type()))
						}
						delQueue = append(delQueue, inst)
					}
				}
				if !broken {
					coroDebugPrintln("did not break", f.Name(), bb.AsValue().Name())
				}
			}

			for _, v := range delQueue {
				v.EraseFromParentAsInstruction()
			}

			nonReturning[f] = true
		}
	}

	// convert direct calls into an async call followed by a yield operation
	coroDebugPrintln("convert direct calls into an async call followed by a yield operation")
	for _, f := range asyncList {
		if f == yield {
			continue
		}
		coroDebugPrintln("scanning", f.Name())

		var retAlloc llvm.Value

		// Rewrite async calls
		for bb := f.EntryBasicBlock(); !bb.IsNil(); bb = llvm.NextBasicBlock(bb) {
			for inst := bb.FirstInstruction(); !inst.IsNil(); inst = llvm.NextInstruction(inst) {
				if !inst.IsACallInst().IsNil() {
					callee := inst.CalledValue()
					if _, ok := asyncFuncs[callee]; !ok || callee == yield {
						continue
					}

					uses := getUses(inst)
					next := llvm.NextInstruction(inst)
					switch {
					case nonReturning[callee]:
						// callee blocks forever
						coroDebugPrintln("optimizing indefinitely blocking call", f.Name(), callee.Name())

						// never calls getCoroutine - coroutine handle is irrelevant
						inst.SetOperand(inst.OperandsCount()-2, llvm.Undef(c.i8ptrType))

						// insert return
						c.builder.SetInsertPointBefore(next)
						c.createRuntimeCall("noret", []llvm.Value{}, "")
						var retInst llvm.Value
						if f.Type().ElementType().ReturnType().TypeKind() == llvm.VoidTypeKind {
							retInst = c.builder.CreateRetVoid()
						} else {
							retInst = c.builder.CreateRet(llvm.Undef(f.Type().ElementType().ReturnType()))
						}

						// delete everything after return
						for next := llvm.NextInstruction(retInst); !next.IsNil(); next = llvm.NextInstruction(retInst) {
							next.ReplaceAllUsesWith(llvm.Undef(retInst.Type()))
							next.EraseFromParentAsInstruction()
						}

						continue
					case next.IsAReturnInst().IsNil():
						// not a return instruction
						coroDebugPrintln("not a return instruction", f.Name(), callee.Name())
					case callee.Type().ElementType().ReturnType() != f.Type().ElementType().ReturnType():
						// return types do not match
						coroDebugPrintln("return types do not match", f.Name(), callee.Name())
					case callee.Type().ElementType().ReturnType().TypeKind() == llvm.VoidTypeKind:
						fallthrough
					case next.Operand(0) == inst:
						// async tail call optimization - just pass parent handle
						coroDebugPrintln("doing async tail call opt", f.Name())

						// insert before call
						c.builder.SetInsertPointBefore(inst)

						// get parent handle
						parentHandle := c.createRuntimeCall("getParentHandle", []llvm.Value{}, "")

						// pass parent handle directly into function
						inst.SetOperand(inst.OperandsCount()-2, parentHandle)

						if inst.Type().TypeKind() != llvm.VoidTypeKind {
							// delete return value
							uses[0].SetOperand(0, llvm.Undef(inst.Type()))
						}

						c.builder.SetInsertPointBefore(next)
						c.createRuntimeCall("yield", []llvm.Value{}, "")
						c.createRuntimeCall("noret", []llvm.Value{}, "")

						continue
					}

					coroDebugPrintln("inserting regular call", f.Name(), callee.Name())
					c.builder.SetInsertPointBefore(inst)

					// insert call to getCoroutine, this will be lowered later
					coro := c.createRuntimeCall("getCoroutine", []llvm.Value{}, "")

					// provide coroutine handle to function
					inst.SetOperand(inst.OperandsCount()-2, coro)

					// Allocate space for the return value.
					var retvalAlloca llvm.Value
					if inst.Type().TypeKind() != llvm.VoidTypeKind {
						if retAlloc.IsNil() {
							// insert at start of function
							c.builder.SetInsertPointBefore(f.EntryBasicBlock().FirstInstruction())

							// allocate return value buffer
							retAlloc = c.builder.CreateAlloca(inst.Type(), "coro.retvalAlloca")
						}
						retvalAlloca = retAlloc

						// call before function
						c.builder.SetInsertPointBefore(inst)

						// cast buffer pointer to *i8
						data := c.builder.CreateBitCast(retvalAlloca, c.i8ptrType, "")

						// set state pointer to return value buffer so it can be written back
						c.createRuntimeCall("setTaskStatePtr", []llvm.Value{coro, data}, "")
					}

					// insert yield after starting function
					c.builder.SetInsertPointBefore(llvm.NextInstruction(inst))
					yieldCall := c.createRuntimeCall("yield", []llvm.Value{}, "")

					if !retvalAlloca.IsNil() && !inst.FirstUse().IsNil() {
						// Load the return value from the alloca.
						// The callee has written the return value to it.
						c.builder.SetInsertPointBefore(llvm.NextInstruction(yieldCall))
						retval := c.builder.CreateLoad(retvalAlloca, "coro.retval")
						inst.ReplaceAllUsesWith(retval)
					}
				}
			}
		}
	}

	// ditch unnecessary tail yields
	coroDebugPrintln("ditch unnecessary tail yields")
	noret := c.mod.NamedFunction("runtime.noret")
	for _, f := range asyncList {
		if f == yield {
			continue
		}
		coroDebugPrintln("scanning", f.Name())

		// we can only ditch a yield if we can ditch all yields
		var yields []llvm.Value
		var canDitch bool
	scanYields:
		for bb := f.EntryBasicBlock(); !bb.IsNil(); bb = llvm.NextBasicBlock(bb) {
			for inst := bb.FirstInstruction(); !inst.IsNil(); inst = llvm.NextInstruction(inst) {
				if inst.IsACallInst().IsNil() || inst.CalledValue() != yield {
					continue
				}

				yields = append(yields, inst)

				// we can only ditch the yield if the next instruction is a void return *or* noret
				next := llvm.NextInstruction(inst)
				ditchable := false
				switch {
				case !next.IsACallInst().IsNil() && next.CalledValue() == noret:
					coroDebugPrintln("ditching yield with noret", f.Name())
					ditchable = true
				case !next.IsAReturnInst().IsNil() && f.Type().ElementType().ReturnType().TypeKind() == llvm.VoidTypeKind:
					coroDebugPrintln("ditching yield with void return", f.Name())
					ditchable = true
				case !next.IsAReturnInst().IsNil():
					coroDebugPrintln("not ditching because return is not void", f.Name(), f.Type().ElementType().ReturnType().String())
				default:
					coroDebugPrintln("not ditching", f.Name())
				}
				if !ditchable {
					// unditchable yield
					canDitch = false
					break scanYields
				}

				// ditchable yield
				canDitch = true
			}
		}

		if canDitch {
			coroDebugPrintln("ditching all in", f.Name())
			for _, inst := range yields {
				if !llvm.NextInstruction(inst).IsAReturnInst().IsNil() {
					// insert noret
					coroDebugPrintln("insering noret", f.Name())
					c.builder.SetInsertPointBefore(inst)
					c.createRuntimeCall("noret", []llvm.Value{}, "")
				}

				// delete original yield
				inst.EraseFromParentAsInstruction()
			}
		}
	}

	// generate return reactivations
	coroDebugPrintln("generate return reactivations")
	for _, f := range asyncList {
		if f == yield {
			continue
		}
		coroDebugPrintln("scanning", f.Name())

		var retPtr llvm.Value
		for bb := f.EntryBasicBlock(); !bb.IsNil(); bb = llvm.NextBasicBlock(bb) {
		block:
			for inst := bb.FirstInstruction(); !inst.IsNil(); inst = llvm.NextInstruction(inst) {
				switch {
				case !inst.IsACallInst().IsNil() && inst.CalledValue() == noret:
					// does not return normally - skip this basic block
					coroDebugPrintln("noret found - skipping", f.Name(), bb.AsValue().Name())
					break block
				case !inst.IsAReturnInst().IsNil():
					// return instruction - rewrite to reactivation
					coroDebugPrintln("adding return reactivation", f.Name(), bb.AsValue().Name())
					if f.Type().ElementType().ReturnType().TypeKind() != llvm.VoidTypeKind {
						// returns something
						if retPtr.IsNil() {
							coroDebugPrintln("adding return pointer get", f.Name())

							// get return pointer in entry block
							c.builder.SetInsertPointBefore(f.EntryBasicBlock().FirstInstruction())
							parentHandle := c.createRuntimeCall("getParentHandle", []llvm.Value{}, "")
							ptr := c.createRuntimeCall("getTaskStatePtr", []llvm.Value{parentHandle}, "")
							retPtr = c.builder.CreateBitCast(ptr, llvm.PointerType(f.Type().ElementType().ReturnType(), 0), "retPtr")
						}

						coroDebugPrintln("adding return store", f.Name(), bb.AsValue().Name())

						// store result into return pointer
						c.builder.SetInsertPointBefore(inst)
						c.builder.CreateStore(inst.Operand(0), retPtr)

						// delete return value
						inst.SetOperand(0, llvm.Undef(inst.Type()))
					}

					// insert reactivation call
					c.builder.SetInsertPointBefore(inst)
					parentHandle := c.createRuntimeCall("getParentHandle", []llvm.Value{}, "")
					c.createRuntimeCall("activateTask", []llvm.Value{parentHandle}, "")

					// mark as noret
					c.builder.SetInsertPointBefore(inst)
					c.createRuntimeCall("noret", []llvm.Value{}, "")
					break block

					// DO NOT ERASE THE RETURN!!!!!!!
				}
			}
		}
	}

	// Create a few LLVM intrinsics for coroutine support.

	coroIdType := llvm.FunctionType(c.ctx.TokenType(), []llvm.Type{c.ctx.Int32Type(), c.i8ptrType, c.i8ptrType, c.i8ptrType}, false)
	coroIdFunc := llvm.AddFunction(c.mod, "llvm.coro.id", coroIdType)

	coroSizeType := llvm.FunctionType(c.ctx.Int32Type(), nil, false)
	coroSizeFunc := llvm.AddFunction(c.mod, "llvm.coro.size.i32", coroSizeType)

	coroBeginType := llvm.FunctionType(c.i8ptrType, []llvm.Type{c.ctx.TokenType(), c.i8ptrType}, false)
	coroBeginFunc := llvm.AddFunction(c.mod, "llvm.coro.begin", coroBeginType)

	coroSuspendType := llvm.FunctionType(c.ctx.Int8Type(), []llvm.Type{c.ctx.TokenType(), c.ctx.Int1Type()}, false)
	coroSuspendFunc := llvm.AddFunction(c.mod, "llvm.coro.suspend", coroSuspendType)

	coroEndType := llvm.FunctionType(c.ctx.Int1Type(), []llvm.Type{c.i8ptrType, c.ctx.Int1Type()}, false)
	coroEndFunc := llvm.AddFunction(c.mod, "llvm.coro.end", coroEndType)

	coroFreeType := llvm.FunctionType(c.i8ptrType, []llvm.Type{c.ctx.TokenType(), c.i8ptrType}, false)
	coroFreeFunc := llvm.AddFunction(c.mod, "llvm.coro.free", coroFreeType)

	// split blocks and add LLVM coroutine intrinsics
	coroDebugPrintln("split blocks and add LLVM coroutine intrinsics")
	for _, f := range asyncList {
		if f == yield {
			continue
		}

		// find calls to yield
		var yieldCalls []llvm.Value
		for bb := f.EntryBasicBlock(); !bb.IsNil(); bb = llvm.NextBasicBlock(bb) {
			for inst := bb.FirstInstruction(); !inst.IsNil(); inst = llvm.NextInstruction(inst) {
				if !inst.IsACallInst().IsNil() && inst.CalledValue() == yield {
					yieldCalls = append(yieldCalls, inst)
				}
			}
		}

		if len(yieldCalls) == 0 {
			// no yields - we do not have to LLVM-ify this
			coroDebugPrintln("skipping", f.Name())
			for bb := f.EntryBasicBlock(); !bb.IsNil(); bb = llvm.NextBasicBlock(bb) {
				for inst := bb.FirstInstruction(); !inst.IsNil(); inst = llvm.NextInstruction(inst) {
					if !inst.IsACallInst().IsNil() && inst.CalledValue() == getCoroutine {
						// no seperate local task - replace getCoroutine with getParentHandle
						c.builder.SetInsertPointBefore(inst)
						inst.ReplaceAllUsesWith(c.createRuntimeCall("getParentHandle", []llvm.Value{}, ""))
						inst.EraseFromParentAsInstruction()
					}
				}
			}
			continue
		}

		coroDebugPrintln("converting", f.Name())

		// get frame data to mess with
		frame := asyncFuncs[f]

		// add basic blocks to put cleanup and suspend code
		frame.cleanupBlock = c.ctx.AddBasicBlock(f, "task.cleanup")
		frame.suspendBlock = c.ctx.AddBasicBlock(f, "task.suspend")

		// at start of function
		c.builder.SetInsertPointBefore(f.EntryBasicBlock().FirstInstruction())
		taskState := c.builder.CreateAlloca(c.getLLVMRuntimeType("taskState"), "task.state")
		stateI8 := c.builder.CreateBitCast(taskState, c.i8ptrType, "task.state.i8")

		// get LLVM-assigned coroutine ID
		id := c.builder.CreateCall(coroIdFunc, []llvm.Value{
			llvm.ConstInt(c.ctx.Int32Type(), 0, false),
			stateI8,
			llvm.ConstNull(c.i8ptrType),
			llvm.ConstNull(c.i8ptrType),
		}, "task.token")

		// allocate buffer for task struct
		size := c.builder.CreateCall(coroSizeFunc, nil, "task.size")
		if c.targetData.TypeAllocSize(size.Type()) > c.targetData.TypeAllocSize(c.uintptrType) {
			size = c.builder.CreateTrunc(size, c.uintptrType, "task.size.uintptr")
		} else if c.targetData.TypeAllocSize(size.Type()) < c.targetData.TypeAllocSize(c.uintptrType) {
			size = c.builder.CreateZExt(size, c.uintptrType, "task.size.uintptr")
		}
		data := c.createRuntimeCall("alloc", []llvm.Value{size}, "task.data")
		if c.needsStackObjects() {
			c.trackPointer(data)
		}

		// invoke llvm.coro.begin intrinsic and save task pointer
		frame.taskHandle = c.builder.CreateCall(coroBeginFunc, []llvm.Value{id, data}, "task.handle")

		// Coroutine cleanup. Free resources associated with this coroutine.
		c.builder.SetInsertPointAtEnd(frame.cleanupBlock)
		mem := c.builder.CreateCall(coroFreeFunc, []llvm.Value{id, frame.taskHandle}, "task.data.free")
		c.createRuntimeCall("free", []llvm.Value{mem}, "")
		c.builder.CreateBr(frame.suspendBlock)

		// Coroutine suspend. A call to llvm.coro.suspend() will branch here.
		c.builder.SetInsertPointAtEnd(frame.suspendBlock)
		c.builder.CreateCall(coroEndFunc, []llvm.Value{frame.taskHandle, llvm.ConstInt(c.ctx.Int1Type(), 0, false)}, "unused")
		returnType := f.Type().ElementType().ReturnType()
		if returnType.TypeKind() == llvm.VoidTypeKind {
			c.builder.CreateRetVoid()
		} else {
			c.builder.CreateRet(llvm.Undef(returnType))
		}

		for _, inst := range yieldCalls {
			// Replace call to yield with a suspension of the coroutine.
			c.builder.SetInsertPointBefore(inst)
			continuePoint := c.builder.CreateCall(coroSuspendFunc, []llvm.Value{
				llvm.ConstNull(c.ctx.TokenType()),
				llvm.ConstInt(c.ctx.Int1Type(), 0, false),
			}, "")
			wakeup := c.splitBasicBlock(inst, llvm.NextBasicBlock(c.builder.GetInsertBlock()), "task.wakeup")
			c.builder.SetInsertPointBefore(inst)
			sw := c.builder.CreateSwitch(continuePoint, frame.suspendBlock, 2)
			sw.AddCase(llvm.ConstInt(c.ctx.Int8Type(), 0, false), wakeup)
			sw.AddCase(llvm.ConstInt(c.ctx.Int8Type(), 1, false), frame.cleanupBlock)
			inst.EraseFromParentAsInstruction()
		}
		ditchQueue := []llvm.Value{}
		for bb := f.EntryBasicBlock(); !bb.IsNil(); bb = llvm.NextBasicBlock(bb) {
			for inst := bb.FirstInstruction(); !inst.IsNil(); inst = llvm.NextInstruction(inst) {
				if !inst.IsACallInst().IsNil() && inst.CalledValue() == getCoroutine {
					// replace getCoroutine calls with the task handle
					inst.ReplaceAllUsesWith(frame.taskHandle)
					ditchQueue = append(ditchQueue, inst)
				}
				if !inst.IsACallInst().IsNil() && inst.CalledValue() == noret {
					// replace tail yield with jump to cleanup, otherwise we end up with undefined behavior
					c.builder.SetInsertPointBefore(inst)
					c.builder.CreateBr(frame.cleanupBlock)
					ditchQueue = append(ditchQueue, inst, llvm.NextInstruction(inst))
				}
			}
		}
		for _, v := range ditchQueue {
			v.EraseFromParentAsInstruction()
		}
	}

	// check for leftover calls to getCoroutine
	if uses := getUses(getCoroutine); len(uses) > 0 {
		useNames := make([]string, len(uses))
		for i, u := range uses {
			useNames[i] = u.InstructionParent().Parent().Name()
		}
		panic("bad use of getCoroutine: " + strings.Join(useNames, ","))
	}

	// rewrite calls to getParentHandle
	for _, inst := range getUses(c.mod.NamedFunction("runtime.getParentHandle")) {
		f := inst.InstructionParent().Parent()
		var parentHandle llvm.Value
		parentHandle = f.LastParam()
		if parentHandle.IsNil() || parentHandle.Name() != "parentHandle" {
			// sanity check
			panic("trying to make exported function async: " + f.Name())
		}
		inst.ReplaceAllUsesWith(parentHandle)
		inst.EraseFromParentAsInstruction()
	}

	// mark functions that do not require a parent handle
	parentNotRequired := map[llvm.Value]bool{}
	activate := c.mod.NamedFunction("runtime.activateTask")
	for _, f := range asyncList {
		if f == yield {
			continue
		}
		var parentHandle llvm.Value
		parentHandle = f.LastParam()
		if parentHandle.IsNil() || parentHandle.Name() != "parentHandle" {
			// sanity check
			panic("trying to make exported function async: " + f.Name())
		}
		coroDebugPrintln("scanning for non-activate usage of parent handle", f.Name())
		var usesNonActivate bool
		for _, v := range getUses(parentHandle) {
			if v.IsACallInst().IsNil() || v.CalledValue() != activate {
				coroDebugPrintln("found non-activate usage of parent handle", f.Name())
				usesNonActivate = true
				break
			}
		}
		if !usesNonActivate {
			coroDebugPrintln("parent handle not required", f.Name())
			parentNotRequired[f] = true
		} else {
			coroDebugPrintln("parent handle required", f.Name())
		}
	}

	// ditch invalid function attributes
	bads := []llvm.Value{c.mod.NamedFunction("runtime.setTaskStatePtr")}
	for _, f := range append(bads, asyncList...) {
		// These properties were added by the functionattrs pass. Remove
		// them, because now we start using the parameter.
		// https://llvm.org/docs/Passes.html#functionattrs-deduce-function-attributes
		for _, kind := range []string{"nocapture", "readnone"} {
			kindID := llvm.AttributeKindID(kind)
			n := f.ParamsCount()
			for i := 0; i <= n; i++ {
				f.RemoveEnumAttributeAtIndex(i, kindID)
			}
		}
	}

	// eliminate noret
	for _, inst := range getUses(noret) {
		inst.EraseFromParentAsInstruction()
	}

	return true, c.lowerMakeGoroutineCalls(parentNotRequired)
}

// Lower runtime.makeGoroutine calls to regular call instructions. This is done
// after the regular goroutine transformations. The started goroutines are
// either non-blocking (in which case they can be called directly) or blocking,
// in which case they will ask the scheduler themselves to be rescheduled.
func (c *Compiler) lowerMakeGoroutineCalls(parentNotRequired map[llvm.Value]bool) error {
	// The following Go code:
	//   go startedGoroutine()
	//
	// Is translated to the following during IR construction, to preserve the
	// fact that this function should be called as a new goroutine.
	//   %0 = call i8* @runtime.makeGoroutine(i8* bitcast (void (i8*, i8*)* @main.startedGoroutine to i8*), i8* undef, i8* null)
	//   %1 = bitcast i8* %0 to void (i8*, i8*)*
	//   call void %1(i8* undef, i8* undef)
	//
	// This function rewrites it to a direct call:
	//   call void @main.startedGoroutine(i8* undef, i8* null)

	if parentNotRequired == nil {
		parentNotRequired = map[llvm.Value]bool{}
	}
	parentNotRequired[c.mod.NamedFunction("runtime.fakeCoroutine")] = true

	makeGoroutine := c.mod.NamedFunction("runtime.makeGoroutine")
	for _, goroutine := range getUses(makeGoroutine) {
		ptrtointIn := goroutine.Operand(0)
		origFunc := ptrtointIn.Operand(0)
		uses := getUses(goroutine)
		if len(uses) != 1 || uses[0].IsAIntToPtrInst().IsNil() {
			return errors.New("expected exactly 1 inttoptr use of runtime.makeGoroutine")
		}
		inttoptrOut := uses[0]
		uses = getUses(inttoptrOut)
		if len(uses) != 1 || uses[0].IsACallInst().IsNil() {
			return errors.New("expected exactly 1 call use of runtime.makeGoroutine bitcast")
		}
		realCall := uses[0]

		// Create call instruction.
		var params []llvm.Value
		for i := 0; i < realCall.OperandsCount()-1; i++ {
			params = append(params, realCall.Operand(i))
		}
		c.builder.SetInsertPointBefore(realCall)
		if parentNotRequired[origFunc] {
			coroDebugPrintln("providing nil parent handle in goroutine call", goroutine.InstructionParent().Parent().Name(), origFunc.Name())
			params[len(params)-1] = llvm.ConstNull(c.i8ptrType)
		} else {
			coroDebugPrintln("providing fake parent handle in goroutine call", goroutine.InstructionParent().Parent().Name(), origFunc.Name())
			params[len(params)-1] = c.createRuntimeCall("getFakeCoroutine", []llvm.Value{}, "") // parent coroutine handle (must not be nil)
		}
		c.builder.CreateCall(origFunc, params, "")
		realCall.EraseFromParentAsInstruction()
		inttoptrOut.EraseFromParentAsInstruction()
		goroutine.EraseFromParentAsInstruction()
	}

	return nil
}
