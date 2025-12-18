/**
 * About Page Controller
 *
 * This controller renders the about page using Odac's skeleton-based view system.
 * Provides information about Odac and its key components.
 *
 * For AJAX requests, only content is returned. For full page loads, skeleton + content.
 */

module.exports = function (Odac) {
  // Set variables for AJAX responses
  Odac.set(
    {
      pageTitle: 'About Odac',
      version: '1.0.0'
    },
    true
  )

  Odac.View.set({
    skeleton: 'main',
    head: 'main',
    header: 'main',
    content: 'about',
    footer: 'main'
  })
}
