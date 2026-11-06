package main

// Embed the IANA time zone database: the runtime image has none, and timeZone
// fields (sync windows, image updates, housekeeping) need it.
import _ "time/tzdata"
