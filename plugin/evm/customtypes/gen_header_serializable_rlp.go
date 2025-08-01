// Code generated by rlpgen. DO NOT EDIT.

package customtypes

import (
	"io"

	"github.com/ava-labs/libevm/rlp"
)

func (obj *HeaderSerializable) EncodeRLP(_w io.Writer) error {
	w := rlp.NewEncoderBuffer(_w)
	_tmp0 := w.List()
	w.WriteBytes(obj.ParentHash[:])
	w.WriteBytes(obj.UncleHash[:])
	w.WriteBytes(obj.Coinbase[:])
	w.WriteBytes(obj.Root[:])
	w.WriteBytes(obj.TxHash[:])
	w.WriteBytes(obj.ReceiptHash[:])
	w.WriteBytes(obj.Bloom[:])
	if obj.Difficulty == nil {
		w.Write(rlp.EmptyString)
	} else {
		if obj.Difficulty.Sign() == -1 {
			return rlp.ErrNegativeBigInt
		}
		w.WriteBigInt(obj.Difficulty)
	}
	if obj.Number == nil {
		w.Write(rlp.EmptyString)
	} else {
		if obj.Number.Sign() == -1 {
			return rlp.ErrNegativeBigInt
		}
		w.WriteBigInt(obj.Number)
	}
	w.WriteUint64(obj.GasLimit)
	w.WriteUint64(obj.GasUsed)
	w.WriteUint64(obj.Time)
	w.WriteBytes(obj.Extra)
	w.WriteBytes(obj.MixDigest[:])
	w.WriteBytes(obj.Nonce[:])
	_tmp1 := obj.BaseFee != nil
	_tmp2 := obj.BlockGasCost != nil
	_tmp3 := len(obj.TxDependency) > 0
	_tmp4 := obj.BlobGasUsed != nil
	_tmp5 := obj.ExcessBlobGas != nil
	_tmp6 := obj.ParentBeaconRoot != nil
	if _tmp1 || _tmp2 || _tmp3 || _tmp4 || _tmp5 || _tmp6 {
		if obj.BaseFee == nil {
			w.Write(rlp.EmptyString)
		} else {
			if obj.BaseFee.Sign() == -1 {
				return rlp.ErrNegativeBigInt
			}
			w.WriteBigInt(obj.BaseFee)
		}
	}
	if _tmp2 || _tmp3 || _tmp4 || _tmp5 || _tmp6 {
		if obj.BlockGasCost == nil {
			w.Write(rlp.EmptyString)
		} else {
			if obj.BlockGasCost.Sign() == -1 {
				return rlp.ErrNegativeBigInt
			}
			w.WriteBigInt(obj.BlockGasCost)
		}
	}
	if _tmp3 || _tmp4 || _tmp5 || _tmp6 {
		_tmp7 := w.List()
		for _, _tmp8 := range obj.TxDependency {
			_tmp9 := w.List()
			for _, _tmp10 := range _tmp8 {
				w.WriteUint64(_tmp10)
			}
			w.ListEnd(_tmp9)
		}
		w.ListEnd(_tmp7)
	}
	if _tmp4 || _tmp5 || _tmp6 {
		if obj.BlobGasUsed == nil {
			w.Write([]byte{0x80})
		} else {
			w.WriteUint64((*obj.BlobGasUsed))
		}
	}
	if _tmp5 || _tmp6 {
		if obj.ExcessBlobGas == nil {
			w.Write([]byte{0x80})
		} else {
			w.WriteUint64((*obj.ExcessBlobGas))
		}
	}
	if _tmp6 {
		if obj.ParentBeaconRoot == nil {
			w.Write([]byte{0x80})
		} else {
			w.WriteBytes(obj.ParentBeaconRoot[:])
		}
	}
	w.ListEnd(_tmp0)
	return w.Flush()
}
