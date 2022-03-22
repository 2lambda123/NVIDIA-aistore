// msgp -file <path to dsort/extract/shard.go> -tests=false -marshal=false -unexported
// Code generated by the command above; see docs/msgp.md. DO NOT EDIT.
package extract

// Code generated by github.com/tinylib/msgp DO NOT EDIT.

import (
	"github.com/tinylib/msgp/msgp"
)

// DecodeMsg implements msgp.Decodable
func (z *Shard) DecodeMsg(dc *msgp.Reader) (err error) {
	var field []byte
	_ = field
	var zb0001 uint32
	zb0001, err = dc.ReadMapHeader()
	if err != nil {
		err = msgp.WrapError(err)
		return
	}
	for zb0001 > 0 {
		zb0001--
		field, err = dc.ReadMapKeyPtr()
		if err != nil {
			err = msgp.WrapError(err)
			return
		}
		switch msgp.UnsafeString(field) {
		case "s":
			z.Size, err = dc.ReadInt64()
			if err != nil {
				err = msgp.WrapError(err, "Size")
				return
			}
		case "r":
			if dc.IsNil() {
				err = dc.ReadNil()
				if err != nil {
					err = msgp.WrapError(err, "Records")
					return
				}
				z.Records = nil
			} else {
				if z.Records == nil {
					z.Records = new(Records)
				}
				err = z.Records.DecodeMsg(dc)
				if err != nil {
					err = msgp.WrapError(err, "Records")
					return
				}
			}
		case "n":
			z.Name, err = dc.ReadString()
			if err != nil {
				err = msgp.WrapError(err, "Name")
				return
			}
		default:
			err = dc.Skip()
			if err != nil {
				err = msgp.WrapError(err)
				return
			}
		}
	}
	return
}

// EncodeMsg implements msgp.Encodable
func (z *Shard) EncodeMsg(en *msgp.Writer) (err error) {
	// map header, size 3
	// write "s"
	err = en.Append(0x83, 0xa1, 0x73)
	if err != nil {
		return
	}
	err = en.WriteInt64(z.Size)
	if err != nil {
		err = msgp.WrapError(err, "Size")
		return
	}
	// write "r"
	err = en.Append(0xa1, 0x72)
	if err != nil {
		return
	}
	if z.Records == nil {
		err = en.WriteNil()
		if err != nil {
			return
		}
	} else {
		err = z.Records.EncodeMsg(en)
		if err != nil {
			err = msgp.WrapError(err, "Records")
			return
		}
	}
	// write "n"
	err = en.Append(0xa1, 0x6e)
	if err != nil {
		return
	}
	err = en.WriteString(z.Name)
	if err != nil {
		err = msgp.WrapError(err, "Name")
		return
	}
	return
}

// Msgsize returns an upper bound estimate of the number of bytes occupied by the serialized message
func (z *Shard) Msgsize() (s int) {
	s = 1 + 2 + msgp.Int64Size + 2
	if z.Records == nil {
		s += msgp.NilSize
	} else {
		s += z.Records.Msgsize()
	}
	s += 2 + msgp.StringPrefixSize + len(z.Name)
	return
}
