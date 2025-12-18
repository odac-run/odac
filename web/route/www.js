// ============================================
// Page Routes
// ============================================
// Page routes render HTML views and support AJAX loading via odac.js
// Controllers are located in controller/page/ directory

// Home page - displays welcome message, features, and interactive demos
Odac.Route.page('/', 'index')

// About page - provides information about Odac
Odac.Route.page('/about', 'about')

// ============================================
// API Routes
// ============================================
// Add your API routes here
// Example:
// Odac.Route.post('/api/contact', 'contact')
// Odac.Route.get('/api/data', 'data')
