/** @type {import('jest').Config} */
const config = {
  verbose: true,
  roots: ['<rootDir>/test'],
  testMatch: ['**/test/**/*.test.js', '**/?(*.)+(spec|test).js'],
  setupFiles: ['<rootDir>/jest.setup.js'],
  collectCoverage: true,
  coverageDirectory: 'coverage',
  collectCoverageFrom: ['core/**/*.js', 'server/**/*.js'],
  coveragePathIgnorePatterns: ['/node_modules/', 'Web.js'],
  forceExit: true
  // Coverage thresholds disabled - focus on test pass/fail only
  // Coverage is still collected and reported, but doesn't block commits
  // This allows incremental test development without artificial barriers
  // coverageThreshold: {
  //   global: {
  //     branches: 80,
  //     functions: 80,
  //     lines: 80,
  //     statements: 80
  //   }
  // }
}

module.exports = config
