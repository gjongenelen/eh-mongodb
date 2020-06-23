[![Build Status](https://travis-ci.com/gjongenelen/eh-mongodb.svg?branch=master)](https://travis-ci.com/gjongenelen/eh-mongodb)
[![codecov](https://codecov.io/gh/gjongenelen/eh-mongodb/branch/master/graph/badge.svg)](https://codecov.io/gh/gjongenelen/eh-mongodb)
[![GoDoc](https://godoc.org/github.com/gjongenelen/eh-mongodb?status.svg)](https://godoc.org/github.com/gjongenelen/eh-mongodb)
[![Go Report Card](https://goreportcard.com/badge/seedboxtech/eh-dynamo)](https://goreportcard.com/report/gjongenelen/eh-mongodb)

# eh-mongodb

This package is based on the default mongo-driver in [EventHorizon](https://github.com/looplab/eventhorizon). 
Mongo has a [document limit of 16MB](https://docs.mongodb.com/manual/reference/limits/) which can easily be reached in big projects, resulting in aggregates with many events not being saved. 

The default mongo-driver in EventHorizon stores an aggregate with its events in one single document, increasing the size of the document on each event. This driver creates a new document per event, preventing documents from growing and reaching the limit mentioned above.
