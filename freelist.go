package main

import "encoding/binary"

// node format:
// |next| pointers | unused
type LNode []byte

const FREE_LIST_HEADER = 8
const FREE_LIST_CAP = (BTREE_PAGE_SIZE - FREE_LIST_HEADER) / 8

// settin& gettin
func (node LNode) getNext() uint64 {
	return binary.LittleEndian.Uint64(node[0:8])
}
func (node LNode) setNext(next uint64) {
	binary.LittleEndian.PutUint64(node[0:8], next)
}
func (node LNode) getPtr(idx int) uint64 {
	offset := FREE_LIST_HEADER + 8*idx
	return binary.LittleEndian.Uint64(node[offset:])
}
func (node LNode) setPtr(idx int, ptr uint64) {
	assert(idx < FREE_LIST_CAP)
	offset := FREE_LIST_HEADER + 8*idx
	binary.LittleEndian.PutUint64(node[offset:], ptr)
}

type FreeList struct {
	// read a page
	get func(uint64) []byte
	// updating an existing page
	set func(uint64) []byte
	// append a new page
	new func([]byte) uint64

	// pointer to head node
	headPage uint64
	// seq. no. to index into list head
	headSeq  uint64
	tailPage uint64
	tailSeq  uint64

	// in-memory states
	maxSeq uint64 // saved tailSeq to prevnt consuming newly added items
}

func seq2idx(seq uint64) int {
	return int(seq % FREE_LIST_CAP)
}

func (fl *FreeList) SetMaxSeq() {
	fl.maxSeq = fl.tailSeq
}

// get 1 item form list head
func (fl *FreeList) PopHead() uint64 {
    ptr, head := flPop(fl)
    if head != 0 {
        fl.PushTail(head)
    }

    return ptr
}

func (fl *FreeList) PushTail(ptr uint64) {
    // addin to tail node
    LNode(fl.set(fl.tailPage)).setPtr(seq2idx(fl.tailSeq), ptr)
    fl.tailSeq ++

    if seq2idx(fl.tailSeq) == 0 {
        next, head := flPop(fl)
        if next == 0 {
            // allocate new node by appending
            next = fl.new(make([]byte, BTREE_PAGE_SIZE))
        }

        // link to new tail node
        LNode(fl.set(fl.tailPage)).setNext(next)
        fl.tailPage = next

        // add head node if its removed
        if head != 0 {
            LNode(fl.set(fl.tailPage)).setPtr(0, head)
            fl.tailSeq++       
        }
    }
}

// rmeove 1 item from head node & remove head node if empty
func flPop(fl *FreeList) (ptr uint64, head uint64) {
	if fl.headSeq == fl.maxSeq {
		return 0, 0
	}

	node := LNode(fl.get(fl.headPage))
	ptr = node.getPtr(seq2idx(fl.headSeq))
	fl.headSeq++

	if seq2idx(fl.headSeq) == 0 {
		head, fl.headPage = fl.headPage, node.getNext()
		assert(fl.headPage != 0)
	}

	return
}