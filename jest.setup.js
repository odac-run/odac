// Jest setup file - runs before each test file
// This ensures global.Odac is always defined for coverage instrumentation

const {mockOdac} = require('./test/server/__mocks__/globalOdac')

// Set global Odac mock
global.Odac = mockOdac
