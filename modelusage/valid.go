package modelusage

// validJSON reports whether data is one JSON value with optional white space around it, by the grammar of RFC
// 8259 as encoding/json reads it (a string may hold invalid UTF-8). It keeps one byte per open object or array
// and never recurses, so nesting has no limit of its own: the byte limits bound it. encoding/json refuses
// nesting deeper than 10,000 levels, which would let an unknown member hide the usage of a valid reply.
func validJSON(data []byte) bool {
	var (
		stackSpace [64]byte
		stack      = stackSpace[:0]
	)
	i := skipSpace(data, 0)
	for {
		// A value starts at i.
		if i >= len(data) {
			return false
		}
		switch c := data[i]; {
		case c == '{' || c == '[':
			i = skipSpace(data, i+1)
			if i < len(data) && data[i] == closing(c) {
				i++
				break
			}
			stack = append(stack, c)
			if c == '{' {
				if i = key(data, i); i < 0 {
					return false
				}
			}
			continue
		case c == '"':
			if i = stringEnd(data, i); i < 0 {
				return false
			}
		case c == '-' || c >= '0' && c <= '9':
			if i = numberEnd(data, i); i < 0 {
				return false
			}
		default:
			if i = literalEnd(data, i); i < 0 {
				return false
			}
		}
		// A value ended before i: close containers or move to the next member or element.
		for {
			i = skipSpace(data, i)
			if len(stack) == 0 {
				return i == len(data)
			}
			if i >= len(data) {
				return false
			}
			top := stack[len(stack)-1]
			if data[i] == closing(top) {
				stack = stack[:len(stack)-1]
				i++
				continue
			}
			if data[i] != ',' {
				return false
			}
			i = skipSpace(data, i+1)
			if top == '{' {
				if i = key(data, i); i < 0 {
					return false
				}
			}
			break
		}
	}
}

// closing returns the byte that closes the object or array opened by open.
func closing(open byte) byte {
	if open == '{' {
		return '}'
	}
	return ']'
}

// key reads a member name and its colon at data[i] and returns the index of the value after them, or -1.
func key(data []byte, i int) int {
	if i >= len(data) || data[i] != '"' {
		return -1
	}
	if i = stringEnd(data, i); i < 0 {
		return -1
	}
	i = skipSpace(data, i)
	if i >= len(data) || data[i] != ':' {
		return -1
	}
	return skipSpace(data, i+1)
}

// stringEnd returns the index after the string that starts with the quote at data[i], or -1 when it is not a
// valid JSON string.
func stringEnd(data []byte, i int) int {
	for j := i + 1; j < len(data); j++ {
		switch c := data[j]; {
		case c == '"':
			return j + 1
		case c < ' ':
			return -1
		case c == '\\':
			j++
			if j >= len(data) {
				return -1
			}
			switch data[j] {
			case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
			case 'u':
				if j+4 >= len(data) {
					return -1
				}
				for _, h := range data[j+1 : j+5] {
					if !isHex(h) {
						return -1
					}
				}
				j += 4
			default:
				return -1
			}
		}
	}
	return -1
}

func isHex(c byte) bool { return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F' }

// numberEnd returns the index after the number at data[i], or -1 when it is not a valid JSON number.
func numberEnd(data []byte, i int) int {
	if data[i] == '-' {
		i++
	}
	switch {
	case i < len(data) && data[i] == '0':
		i++
	case i < len(data) && data[i] >= '1' && data[i] <= '9':
		i = digitsEnd(data, i)
	default:
		return -1
	}
	if i < len(data) && data[i] == '.' {
		j := digitsEnd(data, i+1)
		if j == i+1 {
			return -1
		}
		i = j
	}
	if i < len(data) && (data[i] == 'e' || data[i] == 'E') {
		i++
		if i < len(data) && (data[i] == '+' || data[i] == '-') {
			i++
		}
		j := digitsEnd(data, i)
		if j == i {
			return -1
		}
		i = j
	}
	return i
}

func digitsEnd(data []byte, i int) int {
	for i < len(data) && data[i] >= '0' && data[i] <= '9' {
		i++
	}
	return i
}

// literalEnd returns the index after true, false or null at data[i], or -1.
func literalEnd(data []byte, i int) int {
	for _, lit := range [...]string{"true", "false", "null"} {
		if len(data)-i >= len(lit) && string(data[i:i+len(lit)]) == lit {
			return i + len(lit)
		}
	}
	return -1
}
