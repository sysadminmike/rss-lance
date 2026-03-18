/** @type {import('jest').Config} */
module.exports = {
  testEnvironment: 'jsdom',
  roots: ['../tests/frontend'],
  testMatch: ['**/*.test.js'],
  // Transform ES modules for jest
  transform: {},
  // We don't use ES module imports in tests - tests use require-style or inline code
  // to avoid needing babel/transforms for the simple vanilla JS being tested
};
