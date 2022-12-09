package bar

type myNoCopy1 struct{}    //@ used(true)
type myNoCopy2 struct{}    //@ used(true)
type stdlibNoCopy struct{} //@ used(true)
type locker struct{}       //@ used(false)
type someStruct struct {   //@ used(false)
	x int
}

func (myNoCopy1) Lock()      {} //@ used(true)
func (recv myNoCopy2) Lock() {} //@ used(true)
func (locker) Lock()         {} //@ used(false)
func (locker) Foobar()       {} //@ used(false)
func (someStruct) Lock()     {} //@ used(false)

func (stdlibNoCopy) Lock()   {} //@ used(true)
func (stdlibNoCopy) Unlock() {} //@ used(true)

type T struct { //@ used(true)
	noCopy1 myNoCopy1    //@ used(true)
	noCopy2 myNoCopy2    //@ used(true)
	noCopy3 stdlibNoCopy //@ used(true)
	field1  someStruct   //@ used(false)
	field2  locker       //@ used(false)
	field3  int          //@ used(false)
}
