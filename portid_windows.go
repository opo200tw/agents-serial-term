package main

// statPortID has no node to look at on Windows (COM<n>): the node check
// always sees the port it opened.
func statPortID(string) (portID, error) { return portID{}, nil }
