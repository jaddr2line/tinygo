// +build scheduler.asyncify

package task

import (
	"unsafe"
)

// state is a structure which holds a reference to the state of the task.
// When the task is suspended, the stack pointers are saved here.
type state struct {
	// entry is the entry function of the task.
	// This is needed every time the function is invoked so that asyncify knows what to rewind.
	entry uintptr

	// args are a pointer to a struct holding the arguments of the function.
	args unsafe.Pointer

	// stackState is the state of the stack while unwound.
	stackState
}

// stackState is the saved state of a stack while unwound.
// The stack is arranged with asyncify at the bottom, C stack at the top, and a gap of available stack space between the two.
type stackState struct {
	// asyncify is the stack pointer of the asyncify stack.
	// This starts from the bottom and grows upwards.
	asyncifysp uintptr

	// asyncify is stack pointer of the C stack.
	// This starts from the top and grows downwards.
	csp uintptr
}

// start creates and starts a new goroutine with the given function and arguments.
// The new goroutine is immediately started.
func start(fn uintptr, args unsafe.Pointer, stackSize uintptr) {
	t := &Task{}
	t.state.initialize(fn, args, stackSize)
	prev := currentTask
	currentTask = t
	t.state.launch()
	currentTask = prev
}

//export tinygo_launch
func (*state) launch()

// initialize the state and prepare to call the specified function with the specified argument bundle.
func (s *state) initialize(fn uintptr, args unsafe.Pointer, stackSize uintptr) {
	// Save the entry call.
	s.entry = fn
	s.args = args

	// Create a stack.
	stack := make([]uintptr, stackSize/unsafe.Sizeof(uintptr(0)))

	// Calculate stack base addresses.
	s.asyncifysp = uintptr(unsafe.Pointer(&stack[0]))
	s.csp = uintptr(unsafe.Pointer(&stack[len(stack)-1])) + unsafe.Sizeof(uintptr(0)) - 1
}

//go:linkname runqueuePushBack runtime.runqueuePushBack
func runqueuePushBack(*Task)

// currentTask is the current running task, or nil if currently in the scheduler.
var currentTask *Task

// Current returns the current active task.
func Current() *Task {
	return currentTask
}

// Pause suspends the current task and returns to the scheduler.
// This function may only be called when running on a goroutine stack, not when running on the system stack.
func Pause() {
	println("pausing", currentTask)
	currentTask.state.unwind()
}

//export tinygo_unwind
func (*stackState) unwind()

// Resume the task until it pauses or completes.
// This may only be called from the scheduler.
func (t *Task) Resume() {
	println("resume", t)
	currentTask = t
	t.state.rewind()
	currentTask = nil
	println("rewound", t)
}

//export tinygo_rewind
func (*state) rewind()

// OnSystemStack returns whether the caller is running on the system stack.
func OnSystemStack() bool {
	// If there is not an active goroutine, then this must be running on the system stack.
	return Current() == nil
}
