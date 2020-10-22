package filesys

import (
	"bytes"
	"io"

	"github.com/chrislusf/seaweedfs/weed/util"
	"github.com/valyala/bytebufferpool"
)

type IntervalNode struct {
	Data   []byte
	Offset int64
	Size   int64
	Next   *IntervalNode
	Buffer *bytebufferpool.ByteBuffer
}

func (l *IntervalNode) Bytes() []byte {
	data := l.Data
	if data == nil {
		data = l.Buffer.Bytes()
	}
	return data
}

type IntervalLinkedList struct {
	Head *IntervalNode
	Tail *IntervalNode
}

type ContinuousIntervals struct {
	lists []*IntervalLinkedList
}

func NewIntervalLinkedList(head, tail *IntervalNode) *IntervalLinkedList {
	list := &IntervalLinkedList{
		Head: head,
		Tail: tail,
	}
	return list
}

func (list *IntervalLinkedList) Destroy() {
	for t := list.Head; t != nil; t = t.Next {
		if t.Buffer != nil {
			bytebufferpool.Put(t.Buffer)
		}
	}
}

func (list *IntervalLinkedList) Offset() int64 {
	return list.Head.Offset
}
func (list *IntervalLinkedList) Size() int64 {
	return list.Tail.Offset + list.Tail.Size - list.Head.Offset
}

func (list *IntervalLinkedList) addNodeToTail(node *IntervalNode) {
	// glog.V(4).Infof("add to tail [%d,%d) + [%d,%d) => [%d,%d)", list.Head.Offset, list.Tail.Offset+list.Tail.Size, node.Offset, node.Offset+node.Size, list.Head.Offset, node.Offset+node.Size)
	if list.Tail.Buffer == nil {
		list.Tail.Buffer = bytebufferpool.Get()
		list.Tail.Buffer.Write(list.Tail.Data)
		list.Tail.Data = nil
	}
	list.Tail.Buffer.Write(node.Data)
	list.Tail.Size += int64(len(node.Data))
	return
}
func (list *IntervalLinkedList) addNodeToHead(node *IntervalNode) {
	// glog.V(4).Infof("add to head [%d,%d) + [%d,%d) => [%d,%d)", node.Offset, node.Offset+node.Size, list.Head.Offset, list.Tail.Offset+list.Tail.Size, node.Offset, list.Tail.Offset+list.Tail.Size)
	node.Next = list.Head
	list.Head = node
}

func (list *IntervalLinkedList) ReadData(buf []byte, start, stop int64) {
	t := list.Head
	for {

		nodeStart, nodeStop := max(start, t.Offset), min(stop, t.Offset+t.Size)
		if nodeStart < nodeStop {
			// glog.V(0).Infof("copying start=%d stop=%d t=[%d,%d) t.data=%d => bufSize=%d nodeStart=%d, nodeStop=%d", start, stop, t.Offset, t.Offset+t.Size, len(t.Data), len(buf), nodeStart, nodeStop)
			copy(buf[nodeStart-start:], t.Bytes()[nodeStart-t.Offset:nodeStop-t.Offset])
		}

		if t.Next == nil {
			break
		}
		t = t.Next
	}
}

func (c *ContinuousIntervals) TotalSize() (total int64) {
	for _, list := range c.lists {
		total += list.Size()
	}
	return
}

func subList(list *IntervalLinkedList, start, stop int64) *IntervalLinkedList {
	var nodes []*IntervalNode
	for t := list.Head; t != nil; t = t.Next {
		nodeStart, nodeStop := max(start, t.Offset), min(stop, t.Offset+t.Size)
		if nodeStart >= nodeStop {
			// skip non overlapping IntervalNode
			continue
		}
		data := t.Bytes()[nodeStart-t.Offset : nodeStop-t.Offset]
		if t.Data == nil {
			// need to clone if the bytes is from byte buffer
			t := make([]byte, len(data))
			copy(t, data)
			data = t
		}
		nodes = append(nodes, &IntervalNode{
			Data:   data,
			Offset: nodeStart,
			Size:   nodeStop - nodeStart,
			Next:   nil,
		})
	}
	for i := 1; i < len(nodes); i++ {
		nodes[i-1].Next = nodes[i]
	}
	return NewIntervalLinkedList(nodes[0], nodes[len(nodes)-1])
}

func (c *ContinuousIntervals) AddInterval(data []byte, offset int64) {

	interval := &IntervalNode{Data: data, Offset: offset, Size: int64(len(data))}

	var newLists []*IntervalLinkedList
	for _, list := range c.lists {
		// if list is to the left of new interval, add to the new list
		if list.Tail.Offset+list.Tail.Size <= interval.Offset {
			newLists = append(newLists, list)
		}
		// if list is to the right of new interval, add to the new list
		if interval.Offset+interval.Size <= list.Head.Offset {
			newLists = append(newLists, list)
		}
		// if new interval overwrite the right part of the list
		if list.Head.Offset < interval.Offset && interval.Offset < list.Tail.Offset+list.Tail.Size {
			// create a new list of the left part of existing list
			newLists = append(newLists, subList(list, list.Offset(), interval.Offset))
		}
		// if new interval overwrite the left part of the list
		if list.Head.Offset < interval.Offset+interval.Size && interval.Offset+interval.Size < list.Tail.Offset+list.Tail.Size {
			// create a new list of the right part of existing list
			newLists = append(newLists, subList(list, interval.Offset+interval.Size, list.Tail.Offset+list.Tail.Size))
		}
		// skip anything that is fully overwritten by the new interval
	}

	c.lists = newLists
	// add the new interval to the lists, connecting neighbor lists
	var prevList, nextList *IntervalLinkedList

	for _, list := range c.lists {
		if list.Head.Offset == interval.Offset+interval.Size {
			nextList = list
			break
		}
	}

	for _, list := range c.lists {
		if list.Head.Offset+list.Size() == offset {
			list.addNodeToTail(interval)
			prevList = list
			break
		}
	}

	if prevList != nil && nextList != nil {
		// glog.V(4).Infof("connecting [%d,%d) + [%d,%d) => [%d,%d)", prevList.Head.Offset, prevList.Tail.Offset+prevList.Tail.Size, nextList.Head.Offset, nextList.Tail.Offset+nextList.Tail.Size, prevList.Head.Offset, nextList.Tail.Offset+nextList.Tail.Size)
		prevList.Tail.Next = nextList.Head
		prevList.Tail = nextList.Tail
		c.removeList(nextList)
	} else if nextList != nil {
		// add to head was not done when checking
		nextList.addNodeToHead(interval)
	}
	if prevList == nil && nextList == nil {
		c.lists = append(c.lists, NewIntervalLinkedList(interval, interval))
	}

	return
}

func (c *ContinuousIntervals) RemoveLargestIntervalLinkedList() *IntervalLinkedList {
	var maxSize int64
	maxIndex, maxOffset := -1, int64(-1)
	for k, list := range c.lists {
		listSize := list.Size()
		if maxSize < listSize || (maxSize == listSize && list.Offset() < maxOffset ) {
			maxSize = listSize
			maxIndex, maxOffset = k, list.Offset()
		}
	}
	if maxSize <= 0 {
		return nil
	}

	t := c.lists[maxIndex]
	c.lists = append(c.lists[0:maxIndex], c.lists[maxIndex+1:]...)
	return t

}

func (c *ContinuousIntervals) removeList(target *IntervalLinkedList) {
	index := -1
	for k, list := range c.lists {
		if list.Offset() == target.Offset() {
			index = k
		}
	}
	if index < 0 {
		return
	}

	c.lists = append(c.lists[0:index], c.lists[index+1:]...)

}

func (c *ContinuousIntervals) ReadDataAt(data []byte, startOffset int64) (maxStop int64) {
	for _, list := range c.lists {
		start := max(startOffset, list.Offset())
		stop := min(startOffset+int64(len(data)), list.Offset()+list.Size())
		if start < stop {
			list.ReadData(data[start-startOffset:], start, stop)
			maxStop = max(maxStop, stop)
		}
	}
	return
}

func (l *IntervalLinkedList) ToReader() io.Reader {
	var readers []io.Reader
	t := l.Head
	readers = append(readers, util.NewBytesReader(t.Bytes()))
	for t.Next != nil {
		t = t.Next
		readers = append(readers, bytes.NewReader(t.Bytes()))
	}
	if len(readers) == 1 {
		return readers[0]
	}
	return io.MultiReader(readers...)
}
