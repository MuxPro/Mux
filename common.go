package mux

// Must panics if the given error is not nil.
// 这是 V2Ray 中常见的错误处理模式，用于简化代码。
// 当一个错误被认为是“绝不应该发生”的情况下，可以用它来包裹函数调用。
func Must(err error) {
	if err != nil {
		panic(err)
	}
}

// Must2 an alias for Must, but for functions that return two values,
// with the second one being an error. It returns the first value.
// 这个函数解决了你遇到的 (int, error) 的问题。
// 它接收一个值和一个 error，如果 error 不为 nil 就 panic，否则返回第一个值。
func Must2[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}
