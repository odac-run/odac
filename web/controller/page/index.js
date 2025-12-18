/**
 * Home Page Controller
 *
 * This controller renders the home page using Odac's skeleton-based view system.
 * The skeleton provides the layout (header, nav, footer) and the view provides the content.
 *
 * For AJAX requests (odac-link navigation), only the content is returned.
 * For full page loads, skeleton + content is returned.
 *
 * This page demonstrates:
 * - Modern, responsive design
 * - odac.js AJAX form handling
 * - odac.js GET requests
 * - Dynamic page loading with odac-link
 */

module.exports = function (Odac) {
  // Set variables that will be available in AJAX responses
  Odac.set(
    {
      welcomeMessage: 'Welcome to Odac!',
      timestamp: Date.now()
    },
    true
  ) // true = include in AJAX responses

  Odac.View.set({
    skeleton: 'main',
    head: 'main',
    header: 'main',
    content: 'home',
    footer: 'main'
  })
}
