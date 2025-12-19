/**
 * Odac Template - Client-Side Application
 *
 * This file demonstrates odac.js features including:
 * - AJAX page loading with odac.loader() for smooth navigation
 * - History API integration
 * - Event delegation
 */

odac.action({
  /**
   * AJAX Navigation
   * Enables smooth page transitions without full page reloads
   *
   * Minimal usage: navigate: 'main'
   * Medium usage: navigate: {update: 'main', on: callback}
   * Full usage: navigate: {links: 'a[href^="/"]', update: {...}, on: callback}
   */
  navigate: {
    update: 'main' // Update <main> element
  },

  /**
   * Custom functions
   * These become available as odac.fn.functionName()
   */
  function: {
    /**
     * Update active navigation state
     * Highlights the current page in the navigation menu
     */
    updateActiveNav: function (url) {
      // Remove active class from all navigation links
      const navLinks = document.querySelectorAll('nav a')
      navLinks.forEach(function (link) {
        link.classList.remove('active')
      })

      // Add active class to current page link
      const currentLink = document.querySelector(`nav a[href="${url}"]`)
      if (currentLink) {
        currentLink.classList.add('active')
      } else if (url === '/' || url === '') {
        // Handle home page
        const homeLink = document.querySelector('nav a[href="/"]')
        if (homeLink) {
          homeLink.classList.add('active')
        }
      }
    }
  },

  /**
   * Initialize application on page load
   * This runs once when the page first loads
   */
  load: function () {
    // Set initial active navigation state
    odac.fn.updateActiveNav(window.location.pathname)
  },

  /**
   * Page-specific initialization
   * These functions run when specific pages are loaded
   */
  page: {
    /**
     * Home page initialization
     */
    index: function () {
      console.log('Home page loaded')
    },

    /**
     * About page initialization
     */
    about: function () {
      console.log('About page loaded')
    },

    /**
     * Docs page initialization
     */
    docs: function () {
      console.log('Docs page loaded')
    }
  }

  // Add your custom event handlers here
  // Example:
  // click: {
  //   '#my-button': function() {
  //     console.log('Button clicked')
  //   }
  // }
})
